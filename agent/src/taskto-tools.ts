import { request } from 'node:http'
import { defineDoc, defineExtension, defineTool, type ToolExecutionApi } from '@earendil-works/pi-durable'
import type { Schema } from '@earendil-works/pi-schema'
import definitions from './taskto-tools.json' with { type: 'json' }
import { testHooks } from './test-hooks.js'

export const ToolAccess = defineDoc({
  kind: 'taskto.tool-access',
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ capability: '' }),
  checkpointWhen: () => true,
})

export function validCapability(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0 && value.length <= 256 && /^[\x21-\x7e]+$/.test(value)
}

const MAX_OUTPUT = 65 << 10
const MAX_REPLY = 128 << 10
const CALL_TIMEOUT_MS = 60_000
const CONNECT_RETRY_MS = 5_000

type Reply = { status: number; body: string }

function post(
  socketPath: string,
  name: string,
  capability: string,
  args: unknown,
  signal?: AbortSignal,
): Promise<Reply> {
  const body = JSON.stringify(args)
  return new Promise((resolve, reject) => {
    const req = request(
      {
        socketPath,
        path: `/tools/${name}`,
        method: 'POST',
        headers: {
          Authorization: `Bearer ${capability}`,
          'Content-Type': 'application/json',
          'Content-Length': Buffer.byteLength(body),
        },
        timeout: CALL_TIMEOUT_MS,
        signal,
      },
      (res) => {
        const chunks: Buffer[] = []
        let size = 0
        res.on('data', (chunk: Buffer) => {
          size += chunk.length
          if (size > MAX_REPLY) {
            req.destroy(new Error('the server replied with more than 128 KiB'))
            return
          }
          chunks.push(chunk)
        })
        res.on('end', () => resolve({ status: res.statusCode ?? 0, body: Buffer.concat(chunks).toString('utf8') }))
        res.on('error', reject)
      },
    )
    req.on('timeout', () => req.destroy(new Error('the server did not answer in time')))
    req.on('error', reject)
    req.end(body)
  })
}

async function callServer(socketPath: string, name: string, capability: string, args: unknown, signal?: AbortSignal) {
  const deadline = Date.now() + CONNECT_RETRY_MS
  for (;;) {
    try {
      return await post(socketPath, name, capability, args, signal)
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code
      if ((code !== 'ENOENT' && code !== 'ECONNREFUSED') || Date.now() > deadline || signal?.aborted) throw error
      await new Promise((resolve) => setTimeout(resolve, 200))
    }
  }
}

function text(value: string, isError = false) {
  return { output: [{ type: 'text' as const, text: value }], ...(isError ? { isError: true } : {}) }
}

function refusal(reply: Reply): string {
  try {
    const parsed = JSON.parse(reply.body) as { error?: unknown }
    if (typeof parsed.error === 'string') return parsed.error
  } catch {}
  return `the server answered ${reply.status}`
}

export type ToolSpec = {
  name: string
  description: string
  read_only: boolean
  conversation_only?: boolean
  no_token?: boolean
  input_schema: Schema<object>
}

function serverTool(socketPath: string, spec: ToolSpec) {
  return defineTool({
    name: spec.name,
    description: spec.description,
    parameters: spec.input_schema,
    replay: spec.read_only ? 'safe' : 'unsafe',
    outputLimits: { maxBytes: MAX_OUTPUT },
    execute: async (args, api: ToolExecutionApi, context) => {
      const access = await api.snapshot(ToolAccess, api.conversationId, context)
      if (!access?.capability) {
        return text(
          'the knowledge base, boards and apps are not available in this conversation yet: they will be from the next message',
          true,
        )
      }
      let reply: Reply
      try {
        reply = await callServer(socketPath, spec.name, access.capability, args, context.abortSignal)
      } catch (error) {
        if (context.abortSignal?.aborted) throw error
        console.error(`tool ${spec.name}: the server is unreachable:`, (error as Error).message)
        return text('the server is unreachable right now: try again shortly', true)
      }
      if (reply.status !== 200) return text(`error: ${refusal(reply)}`, true)
      return text(reply.body)
    },
  })
}

const specs = definitions as ToolSpec[]

export function tasktoTools(socketPath: string, options: { browser?: boolean } = {}) {
  return defineExtension({
    name: 'taskto-tools',
    tools: [...specs, ...testHooks.extraToolSpecs]
      .filter((spec) => options.browser || !spec.conversation_only)
      .map((spec) => serverTool(socketPath, spec)),
  })
}
