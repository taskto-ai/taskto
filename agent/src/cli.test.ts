import { mkdtempSync, rmSync } from 'node:fs'
import { createServer, type Server } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context'
import { createModels } from '@earendil-works/pi-ai/models'
import { fauxAssistantMessage, fauxProvider, fauxText } from '@earendil-works/pi-ai/providers/faux'
import { configure, createRegistry, Harness } from '@earendil-works/pi-durable'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { cliTools, installCliDriver } from './cli.js'
import { openHarnessStorage } from './storage.js'
import { ToolAccess } from './taskto-tools.js'

const context = BACKGROUND_CONTEXT

type Message = { role: string; content?: unknown; toolName?: string; isError?: boolean }

const text = (m: Message | undefined) =>
  typeof m?.content === 'string'
    ? m.content
    : ((m?.content as { type: string; text?: string }[] | undefined) ?? []).map((p) => p.text ?? '').join('')

const NOTE = 'The previous agent (Claude Code) stopped part-way through this request: it reached its usage limit.'

type Done = { status: string; text: string; continue?: boolean; note?: string }

describe('a CLI turn that stopped on a limit and goes on with a model', () => {
  let dir: string
  let server: Server | undefined
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'taskto-cli-continue-'))
  })
  afterEach(async () => {
    await new Promise((resolve) => (server ? server.close(resolve) : resolve(undefined)))
    rmSync(dir, { recursive: true, force: true })
  })

  const run = async (done: Done) => {
    const models = createModels()
    installCliDriver(models, {
      kind: 'cli',
      id: 'cli-claude',
      name: 'Claude Code',
      cli: 'claude',
      models: [{ id: 'default', name: 'default' }],
    })
    const api = fauxProvider({ provider: 'api', models: [{ id: 'big' }] })
    api.setResponses(
      Array.from({ length: 10 }, () => (ctx: { messages: Message[] }) => {
        const result = ctx.messages.findLast((m) => m.role === 'toolResult')
        return fauxAssistantMessage(fauxText(`carrying on after: ${text(result)}`))
      }) as never,
    )
    models.setProvider(api.provider)

    const socket = join(dir, 'tools.sock')
    const registry = createRegistry()
    registry.install(cliTools(socket, async () => [{ role: 'user', content: 'do the thing' }]))
    const harness = await Harness.open(
      await openHarnessStorage(join(dir, 'agent.db')),
      { models, registry, identity: { attribution: null } },
      context,
    )
    harness.resume()
    const conv = await harness.createConversation(
      {
        ownership: { kind: 'ownerless' },
        agent: { model: { provider: 'cli-claude', modelId: 'default' } },
        init: async (tx, id) => {
          ;(await tx.doc(ToolAccess, id)).capability = 'test-capability'
        },
      },
      context,
    )
    server = createServer((req, res) => {
      req.resume()
      req.on('end', () => {
        void (async () => {
          if (done.continue) {
            await conv.commit(async (tx) => {
              await configure(tx, conv.id, { model: { provider: 'api', modelId: 'big' } })
            }, context)
          }
          res.writeHead(200, { 'Content-Type': 'application/x-ndjson' })
          res.write(`${JSON.stringify({ seq: 1, type: 'text', text: 'working on it' })}\n`)
          res.write(`${JSON.stringify({ seq: 2, type: 'notice', text: 'Claude Code reached its usage limit' })}\n`)
          res.end(`${JSON.stringify({ seq: 3, type: 'done', ...done })}\n`)
        })()
      })
    })
    await new Promise<void>((resolve) => server?.listen(socket, resolve))
    const submission = await conv.submit({ type: 'input', content: 'do the thing' }, context)
    await submission.wait(context)
    const messages = [...(await conv.context(context)).messages] as Message[]
    await harness.close(context)
    return messages
  }

  it('lets the new model answer with the hand-off note as what the CLI left', async () => {
    const messages = await run({ status: 'failed', text: 'usage limit', continue: true, note: NOTE })
    const result = messages.findLast((m) => m.role === 'toolResult')
    expect(text(result)).toBe(NOTE)
    expect(result?.isError).toBeFalsy()
    expect(text(messages.at(-1))).toBe(`carrying on after: ${NOTE}`)
    expect(messages.at(-1)?.role).toBe('assistant')
  })

  it('ends with the failure when the server did not say to continue', async () => {
    const messages = await run({ status: 'failed', text: 'usage limit', note: NOTE })
    const result = messages.findLast((m) => m.role === 'toolResult')
    expect(text(result)).toBe('usage limit')
    expect(result?.isError).toBe(true)
    expect(text(messages.at(-1))).toBe('usage limit')
  })

  it('ends with the failure when it said to continue but gave no note', async () => {
    const messages = await run({ status: 'failed', text: 'usage limit', continue: true })
    expect(text(messages.findLast((m) => m.role === 'toolResult'))).toBe('usage limit')
  })
})
