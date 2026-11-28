import { chmodSync, rmSync } from 'node:fs'
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http'
import { connect } from 'node:net'
import { format } from 'node:util'
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context'
import { createModels } from '@earendil-works/pi-ai/models'
import {
  ConversationBusy,
  type ConversationId,
  type Cursor,
  configure,
  createRegistry,
  type EntryId,
  Harness,
  LiveDoc,
  type SubmissionId,
} from '@earendil-works/pi-durable'
import { cliTools } from './cli.js'
import { ContinuePin, latestReplyEntryId, pinHolds } from './continue.js'
import { InputError, type NoteInput, noteEntry, parseNote, type TasktoInputEntry, validInputEntry } from './input.js'
import { Language, LanguageSection, validLanguage } from './language.js'
import { Lifecycle } from './lifecycle.js'
import { ConfigError, hasModel, installProviders, loadProviders, type ModelRef, redact } from './providers.js'
import { purgeInFlight, purgeOnce, startCheckpointer, WalBusyError } from './purge.js'
import { compactOnce, openHarnessStorage, recordFresh } from './storage.js'
import { ToolAccess, tasktoTools, validCapability } from './taskto-tools.js'
import { testHooks } from './test-hooks.js'
import { SpikeTools } from './tools.js'

for (const level of ['error', 'warn', 'log', 'info', 'debug'] as const) {
  const write = console[level].bind(console)
  console[level] = (...args: unknown[]) => write(redact(format(...args)))
}

const socketPath = process.env.TASKTO_AGENT_SOCKET
const dbPath = process.env.TASKTO_AGENT_DB
if (!socketPath || !dbPath) {
  console.error('TASKTO_AGENT_SOCKET and TASKTO_AGENT_DB are required')
  process.exit(2)
}

const taken = await new Promise<boolean>((resolve) => {
  const probe = connect(socketPath)
  probe.once('connect', () => {
    probe.destroy()
    resolve(true)
  })
  probe.once('error', () => resolve(false))
})
if (taken) {
  console.error(`another agent runtime is already listening on ${socketPath}`)
  process.exit(1)
}

const context = BACKGROUND_CONTEXT
const models = createModels()
const providersFile = process.env.TASKTO_AGENT_PROVIDERS
let catalog: ReturnType<typeof installProviders>
try {
  catalog = installProviders(models, loadProviders(providersFile))
} catch (error) {
  if (!(error instanceof ConfigError)) throw error
  console.error(`invalid providers config: ${error.message}`)
  process.exit(2)
}
const registry = createRegistry()
if (testHooks.spikeTools) registry.install(SpikeTools)
registry.install(LanguageSection)
const toolsSocket = process.env.TASKTO_AGENT_TOOLS_SOCKET
if (toolsSocket) {
  registry.install(tasktoTools(toolsSocket, { browser: process.env.TASKTO_AGENT_BROWSER === 'true' }))
  registry.install(
    cliTools(toolsSocket, async (id) => {
      const conv = await harness.conversation(id as ConversationId, context)
      return conv ? (await conv.context(context)).messages : []
    }),
  )
}

const compaction = compactOnce(dbPath as string)
if (compaction === 'compacted') console.error('agent.db compacted once, so content deleted before is gone from it')
const harness = await Harness.open(
  await openHarnessStorage(dbPath as string),
  {
    models,
    registry,
    identity: { attribution: null },
    onReport: (report) => console.error(`report: ${report.kind}`, report.error ?? ''),
  },
  context,
)
if (compaction === 'fresh') recordFresh(dbPath as string)
harness.resume()
startCheckpointer(dbPath as string)
void harness.closed.then((end) => {
  if (end.kind === 'failed') {
    console.error('harness failed; exiting for a restart', end.error)
    process.exit(1)
  }
})

class HttpError extends Error {
  constructor(
    readonly status: number,
    message: string,
    readonly reason?: 'unknown_model' | 'no_model' | 'busy' | 'moved',
  ) {
    super(message)
  }
}

const MAX_BODY = 1 << 20

const walBusyMs = testHooks.walBusyMs ?? 15_000

async function readJson(req: IncomingMessage): Promise<Record<string, unknown>> {
  const chunks: Buffer[] = []
  let size = 0
  for await (const chunk of req) {
    size += (chunk as Buffer).length
    if (size > MAX_BODY) throw new HttpError(413, 'body too large')
    chunks.push(chunk as Buffer)
  }
  const body = Buffer.concat(chunks).toString('utf8')
  if (!body) return {}
  let parsed: unknown
  try {
    parsed = JSON.parse(body)
  } catch {
    throw new HttpError(400, 'invalid JSON body')
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    throw new HttpError(400, 'body must be a JSON object')
  }
  return parsed as Record<string, unknown>
}

function send(res: ServerResponse, status: number, body: unknown): void {
  res.writeHead(status, { 'Content-Type': 'application/json' })
  res.end(redact(JSON.stringify(body)))
}

function modelRef(value: unknown): ModelRef {
  const ref = value as Partial<ModelRef> | null
  if (typeof ref !== 'object' || ref === null || typeof ref.provider !== 'string' || typeof ref.modelId !== 'string') {
    throw new HttpError(400, 'model must be { provider, modelId }')
  }
  const model = { provider: ref.provider, modelId: ref.modelId }
  if (!hasModel(catalog, model)) throw new HttpError(400, 'unknown model', 'unknown_model')
  return model
}

function parseId(segment: string): number {
  const id = /^\d+$/.test(segment) ? Number(segment) : NaN
  if (!Number.isSafeInteger(id)) throw new HttpError(400, 'invalid id')
  return id
}

async function conversation(segment: string) {
  const id = parseId(segment) as ConversationId
  const found = await harness.conversation(id, context)
  if (!found || (await harness.snapshot(Lifecycle, id, context))?.retiredAt != null) {
    throw new HttpError(404, 'unknown conversation')
  }
  return found
}

async function listConversations() {
  const out: { id: number; createdAt: number; retired: boolean }[] = []
  let cursor: Cursor | undefined
  do {
    const page = await harness.commit((tx) => tx.scanConversations({}, 500, cursor), context)
    for (const record of page.items) {
      if (record.owner) continue
      const life = await harness.snapshot(Lifecycle, record.id, context)
      out.push({ id: record.id, createdAt: life?.createdAt ?? 0, retired: life?.retiredAt != null })
    }
    cursor = page.next
  } while (cursor)
  return out
}

const PAGE_ENTRIES = 50
const MAX_PAGE_ENTRIES = 100

async function entries(id: string, query: URLSearchParams) {
  const conv = await conversation(id)
  const before = query.has('before') ? parseId(query.get('before') ?? '') : undefined
  const limit = query.has('limit') ? parseId(query.get('limit') ?? '') : PAGE_ENTRIES
  if (limit < 1 || limit > MAX_PAGE_ENTRIES) throw new HttpError(400, `limit must be 1 to ${MAX_PAGE_ENTRIES}`)
  if (before === 0) return { entries: [] }
  const page = await conv.entries(
    before === undefined ? {} : { maxEntryId: (before - 1) as EntryId },
    limit + 1,
    undefined,
    context,
  )
  const items = page.items.slice(0, limit).reverse()
  return page.items.length > limit ? { entries: items, before: page.items[limit].id } : { entries: items }
}

async function writeLine(res: ServerResponse, line: unknown): Promise<void> {
  if (res.destroyed || res.write(`${redact(JSON.stringify(line))}\n`)) return
  await new Promise<void>((resolve) => {
    const done = () => {
      res.off('drain', done)
      res.off('close', done)
      res.off('error', done)
      resolve()
    }
    res.once('drain', done)
    res.once('close', done)
    res.once('error', done)
  })
}

async function watch(id: string, req: IncomingMessage, res: ServerResponse): Promise<void> {
  const handle = await (await conversation(id)).watch(context)
  const stop = () => handle.stop().catch((error: unknown) => console.error('watch stop failed', error))
  req.on('close', () => void stop())
  res.writeHead(200, { 'Content-Type': 'application/x-ndjson' })
  await writeLine(res, { type: 'value', value: handle.value })
  handle.start(async (call) => {
    if (call.kind !== 'value') return
    try {
      await writeLine(res, { type: 'ops', ops: call.ops })
    } catch (error) {
      console.error('watch write failed', error)
      res.destroy()
      void stop()
    }
  })
  const end = await handle.closed
  if (!res.writableEnded && !res.destroyed) {
    res.end(`${JSON.stringify({ type: 'end', kind: end.kind })}\n`)
  }
}

async function route(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const url = new URL(req.url ?? '/', 'http://agent')
  const parts = url.pathname.split('/').filter(Boolean)
  const [resource, id = '', action] = parts

  if (req.method === 'GET' && url.pathname === '/healthz') return send(res, 200, { status: 'ok', pid: process.pid })

  if (req.method === 'GET' && url.pathname === '/models') return send(res, 200, catalog)

  if (req.method === 'POST' && url.pathname === '/providers/reload') {
    try {
      catalog = installProviders(models, loadProviders(providersFile), catalog)
    } catch (error) {
      if (!(error instanceof ConfigError)) throw error
      console.error(`providers reload refused: ${error.message}`)
      throw new HttpError(400, `invalid providers config: ${error.message}`)
    }
    console.error(`providers reloaded: ${catalog.providers.length}`)
    return send(res, 200, catalog)
  }

  if (req.method === 'POST' && url.pathname === '/conversations') {
    const body = await readJson(req)
    const model = body.model === undefined ? catalog.default : modelRef(body.model)
    const created = await harness.createConversation(
      {
        ownership: { kind: 'ownerless' },
        agent: { model },
        init: async (tx, id) => {
          ;(await tx.doc(Lifecycle, id)).createdAt = Date.now()
        },
      },
      context,
    )
    return send(res, 201, { id: created.id, model })
  }

  if (req.method === 'GET' && url.pathname === '/conversations') {
    return send(res, 200, { conversations: await listConversations() })
  }

  if (resource === 'conversations' && parts.length === 3) {
    if (req.method === 'POST' && action === 'submit') {
      const body = await readJson(req)
      let entry: TasktoInputEntry | undefined
      if (typeof body.content !== 'string') throw new HttpError(400, 'content must be a string')
      const content = body.content
      const whenBusy = body.whenBusy
      if (whenBusy !== undefined && whenBusy !== 'steer' && whenBusy !== 'followUp' && whenBusy !== 'reject') {
        throw new HttpError(400, 'whenBusy must be steer, followUp or reject')
      }
      if (body.requestId !== undefined && typeof body.requestId !== 'string') {
        throw new HttpError(400, 'requestId must be a string')
      }
      if (body.capability !== undefined && !validCapability(body.capability)) {
        throw new HttpError(400, 'capability must be up to 256 printable ASCII characters')
      }
      if (body.language !== undefined && !validLanguage(body.language)) {
        throw new HttpError(400, 'language must be a BCP 47 tag such as "pt-BR", or ""')
      }
      if (body.entry !== undefined) {
        if (!validInputEntry(body.entry)) throw new HttpError(400, 'entry must be a bounded taskto.input entry')
        entry = body.entry
      }
      const conv = await conversation(id)
      const { capability, language } = body as { capability?: string; language?: string }
      let submissionId: SubmissionId | undefined
      await conv.commit(async (tx) => {
        if ((await tx.doc(Lifecycle, conv.id)).retiredAt != null) throw new HttpError(404, 'unknown conversation')
        submissionId = await conv.submit(tx, {
          type: 'input',
          content,
          ...(entry !== undefined ? { entry } : {}),
          ...(whenBusy ? { whenBusy } : {}),
          ...(typeof body.requestId === 'string' ? { requestId: body.requestId } : {}),
        })
        if (capability !== undefined) {
          const access = await tx.doc(ToolAccess, conv.id)
          if (access.capability !== capability) access.capability = capability
        }
        if (language !== undefined) {
          const lang = await tx.doc(Language, conv.id)
          if (lang.tag !== language) lang.tag = language
        }
      }, context)
      return send(res, 202, { submissionId })
    }
    if (req.method === 'POST' && action === 'continue') {
      const body = await readJson(req)
      const model = modelRef(body.model)
      const afterEntry = body.afterEntry
      if (typeof afterEntry !== 'number' || !Number.isSafeInteger(afterEntry) || afterEntry < 1) {
        throw new HttpError(400, 'afterEntry must be the id of the last entry')
      }
      const conv = await conversation(id)
      let submissionId: SubmissionId | undefined
      await testHooks.beforeContinueWrite?.()
      try {
        await conv.commit(async (tx) => {
          if ((await tx.doc(Lifecycle, conv.id)).retiredAt != null) throw new HttpError(404, 'unknown conversation')
          const pin = await tx.doc(ContinuePin, conv.id)
          if (afterEntry <= pin.consumed || (await latestReplyEntryId(tx, conv.id)) !== afterEntry) {
            throw new HttpError(409, 'the conversation moved on', 'moved')
          }
          submissionId = await conv.submit(tx, { type: 'continue' })
          pin.consumed = afterEntry
          pin.submission = submissionId
          await configure(tx, conv.id, { model })
        }, context)
      } catch (error) {
        if (error instanceof ConversationBusy) throw new HttpError(409, 'the conversation is busy', 'busy')
        throw error
      }
      return send(res, 202, { submissionId, model })
    }
    if (req.method === 'POST' && action === 'note') {
      let note: NoteInput
      try {
        note = parseNote(await readJson(req))
      } catch (error) {
        if (error instanceof InputError) throw new HttpError(400, error.message)
        throw error
      }
      const submission = await (await conversation(id)).submit(
        { type: 'write', entry: noteEntry(note), ...(note.requestId ? { requestId: note.requestId } : {}) },
        context,
      )
      return send(res, 202, { submissionId: submission.id })
    }
    if (req.method === 'POST' && action === 'retire') {
      const found = await conversation(id)
      await found.commit(async (tx) => {
        ;(await tx.doc(Lifecycle, found.id)).retiredAt = Date.now()
      }, context)
      await found.abort(context, { background: true })
      console.error(`conversation ${found.id} retired`)
      res.writeHead(204).end()
      return
    }
    if (req.method === 'POST' && action === 'purge') {
      const num = parseId(id) as ConversationId
      const running = purgeInFlight(dbPath as string, num)
      if (running) {
        console.error(`conversation ${num} purge joins the running one`)
        await running
        res.writeHead(204).end()
        return
      }
      const found = await harness.conversation(num, context)
      if (found) {
        if ((await harness.snapshot(Lifecycle, num, context))?.retiredAt == null) {
          throw new HttpError(409, 'retire the conversation before purging it')
        }
        await found.abort(context, { background: true })
        await purgeOnce({ path: dbPath as string, root: num, timeoutMs: walBusyMs })
        console.error(`conversation ${num} purged`)
      } else {
        await purgeOnce({ path: dbPath as string, root: num, timeoutMs: walBusyMs })
      }
      res.writeHead(204).end()
      return
    }
    if (req.method === 'POST' && action === 'abort') {
      await (await conversation(id)).abort(context)
      res.writeHead(204).end()
      return
    }
    if (req.method === 'GET' && action === 'entries') return send(res, 200, await entries(id, url.searchParams))
    if (req.method === 'GET' && action === 'watch') return watch(id, req, res)
    if (req.method === 'GET' && action === 'model') {
      const agent = await (await conversation(id)).agent(context)
      if (!agent.model) throw new HttpError(409, 'conversation has no model', 'no_model')
      return send(res, 200, { provider: agent.model.provider, modelId: agent.model.modelId })
    }
    if (req.method === 'PUT' && action === 'model') {
      const model = modelRef(await readJson(req))
      const conv = await conversation(id)
      await testHooks.beforeModelWrite?.()
      await conv.commit(async (tx) => {
        if ((await tx.doc(Lifecycle, conv.id)).retiredAt != null) throw new HttpError(404, 'unknown conversation')
        if (pinHolds(await tx.doc(ContinuePin, conv.id), await tx.doc(LiveDoc, conv.id))) {
          throw new HttpError(409, 'a continued reply keeps its model until it ends', 'busy')
        }
        await configure(tx, conv.id, { model })
      }, context)
      return send(res, 200, model)
    }
    if (req.method === 'GET' && action === 'messages') {
      const view = await (await conversation(id)).context(context)
      return send(
        res,
        200,
        view.messages.map((m) => ({
          role: m.role,
          text:
            typeof m.content === 'string'
              ? m.content
              : m.content.map((part) => (part.type === 'text' ? part.text : '')).join(''),
        })),
      )
    }
  }

  if (req.method === 'POST' && resource === 'submissions' && action === 'wait' && parts.length === 3) {
    const submission = await harness.submission(parseId(id) as SubmissionId, context)
    if (!submission) throw new HttpError(404, 'unknown submission')
    return send(res, 200, await submission.wait(context))
  }

  throw new HttpError(404, 'not found')
}

const server = createServer((req, res) => {
  route(req, res).catch((error: unknown) => {
    const status = error instanceof HttpError ? error.status : error instanceof WalBusyError ? 503 : 500
    if (status === 500 || status === 503) console.error(error)
    const reason = error instanceof HttpError ? error.reason : undefined
    const message =
      error instanceof HttpError
        ? error.message
        : error instanceof WalBusyError
          ? 'the purge is not done: a reader holds the database'
          : 'internal error'
    if (!res.headersSent) send(res, status, { error: message, ...(reason ? { reason } : {}) })
    else res.end()
  })
})

rmSync(socketPath, { force: true })
const umask = process.umask(0o177)
server.once('error', (error) => {
  process.umask(umask)
  console.error(`agent runtime cannot listen on ${socketPath}:`, error)
  process.exit(1)
})
server.listen(socketPath, () => {
  process.umask(umask)
  chmodSync(socketPath, 0o600)
  console.error(`agent runtime listening on ${socketPath}`)
})

let stopping = false
const shutdown = async () => {
  if (stopping) return
  stopping = true
  server.close()
  server.closeAllConnections()
  const timeout = new Promise<'timeout'>((resolve) => setTimeout(() => resolve('timeout'), 10_000).unref())
  const closed = await Promise.race([harness.close(context).then(() => 'closed' as const), timeout])
  if (closed === 'timeout') console.error('harness did not close within 10 s; exiting anyway')
  process.exit(closed === 'closed' ? 0 : 1)
}
process.once('SIGTERM', () => void shutdown())
process.once('SIGINT', () => void shutdown())
if (process.env.TASKTO_AGENT_EXIT_ON_EOF === '1') {
  process.stdin.on('data', () => {})
  process.stdin.once('end', () => {
    console.error('stdin closed: the server is gone; shutting down')
    void shutdown()
  })
  process.stdin.once('error', () => void shutdown())
}
