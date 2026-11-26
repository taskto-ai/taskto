// Agent runtime sidecar: Pi Durable behind a small HTTP API on a Unix socket.
//
// Only the Go server talks to it. It never listens on TCP and does no auth: the Go server
// checks who may see or steer which conversation before it forwards anything here.
//
//   GET  /healthz
//   GET  /models                             → { providers: [{ id, name, kind, models: [{ id, name }] }], default }
//   POST /providers/reload                   → the new GET /models; 400 and no change if the file is refused
//   GET  /conversations                      → { conversations: [{ id, createdAt, retired }] }, oldest first
//   POST /conversations                      { model?: { provider, modelId } } → { id, model }
//   POST /conversations/:id/retire           → 204: stops its work; from then on it is unknown (404)
//   POST /conversations/:id/purge            → 204: deletes a retired conversation's records from the database
//                                              (409 if it isn't retired; 204 if it is gone already)
//   POST /conversations/:id/abort            → 204 once idle: withdraws queued input and stops the running work
//   GET  /conversations/:id/model            → { provider, modelId }
//   PUT  /conversations/:id/model            { provider, modelId } → { provider, modelId }
//   POST /conversations/:id/submit           { content, whenBusy?, requestId?, capability?, language? } → { submissionId }
//   POST /submissions/:id/wait               → the settled submission
//   GET  /conversations/:id/watch            → NDJSON: { type: "value", value } then { type: "ops", ops } per commit
//   GET  /conversations/:id/messages         → [{ role, text }] of the model's current context
//   GET  /conversations/:id/entries?before=&limit= → { entries, before? }: the page of entries before an ID
//
// With TASKTO_AGENT_EXIT_ON_EOF=1 it shuts down when its stdin closes: the Go supervisor holds
// the pipe's other end, so the sidecar never outlives the server, even one killed with SIGKILL.
//
// Models come from the file named by TASKTO_AGENT_PROVIDERS (docs/agent-providers.md); without
// it only the scripted offline model runs. A model choice outside that list gets a 400. The Go
// server rewrites the file when provider accounts change and asks for a reload, which swaps the
// providers without touching conversations.
//
// With TASKTO_AGENT_TOOLS_SOCKET, the knowledge-base and board tools (taskto-tools.ts) call back
// into the Go server on that socket. A submit's capability is the conversation's for those
// calls: kept for its tool calls, never shown in its view.
//
// Pi Durable never deletes a conversation, so one the server has no use for any more (an orphan,
// left when the server failed to record it) is retired instead: its work is aborted and every
// route answers 404 for it, as for one that never existed. Its records stay in the database
// until the server, for an account that was deleted, asks for them to be purged.
//
// The watch stream carries Chord operations as they are. A client keeps the view current with
// applyImmutable(view, ops) from @earendil-works/chord/delta, so nothing in between has to understand them.

import { chmodSync, rmSync } from 'node:fs'
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http'
import { connect } from 'node:net'
import { format } from 'node:util'
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context'
import { createModels } from '@earendil-works/pi-ai/models'
import {
  type ConversationId,
  type Cursor,
  createRegistry,
  defineDoc,
  type EntryId,
  Harness,
  type SubmissionId,
} from '@earendil-works/pi-durable'
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node'
import { cliTools } from './cli.js'
import { Language, LanguageSection, validLanguage } from './language.js'
import { ConfigError, hasModel, installProviders, loadProviders, type ModelRef, redact } from './providers.js'
import { HARNESS_DB_OPTIONS, LIFECYCLE_KIND, runPurge, startCheckpointer, WalBusyError } from './purge.js'
import { ToolAccess, tasktoTools, validCapability } from './taskto-tools.js'
import { testHooks } from './test-hooks.js'
import { SpikeTools } from './tools.js'

// Everything this process logs passes redact(), so an API key never reaches its output.
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

// One process per storage: refuse to start while another sidecar answers on the socket,
// rather than removing its socket and opening the same database twice. This catches an
// accidental second start; two starting in the same instant could still race, so the
// guarantee is the Go server, which supervises exactly one sidecar per database.
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
// The pause tools exist for the integration tests, which start the sidecar through test-entry.ts;
// nothing in the environment turns them on.
if (testHooks.spikeTools) registry.install(SpikeTools)
// The language the agent replies in (language.ts), from the language each message carries.
registry.install(LanguageSection)
const toolsSocket = process.env.TASKTO_AGENT_TOOLS_SOCKET
if (toolsSocket) {
  registry.install(tasktoTools(toolsSocket, { browser: process.env.TASKTO_AGENT_BROWSER === 'true' }))
  // The CLI workers' tool reads its conversation, which a tool's own handle can't: through the
  // Harness, opened below before any tool runs.
  registry.install(
    cliTools(toolsSocket, async (id) => {
      const conv = await harness.conversation(id as ConversationId, context)
      return conv ? (await conv.context(context)).messages : []
    }),
  )
}

// The commits don't checkpoint the log (HARNESS_DB_OPTIONS): a thread of its own does, so a log a
// purge or a reader let grow is never copied on this thread.
const harness = await Harness.open(
  await openNodeSqliteStorage(dbPath, HARNESS_DB_OPTIONS),
  {
    models,
    registry,
    identity: { attribution: null },
    onReport: (report) => console.error(`report: ${report.kind}`, report.error ?? ''),
  },
  context,
)
harness.resume() // synchronous: continue whatever the last process left unfinished
startCheckpointer(dbPath as string)
// A storage failure closes the Harness for good; exit so the supervisor starts a fresh process,
// which reopens the storage and recovers (Pi Durable's documented restart pattern).
void harness.closed.then((end) => {
  if (end.kind === 'failed') {
    console.error('harness failed; exiting for a restart', end.error)
    process.exit(1)
  }
})

// What the server needs to know of a conversation besides Pi Durable's record: when it was created,
// so a sweep for orphans leaves alone one the server is still recording (0 for one created before
// this document existed), and when it was retired.
const Lifecycle = defineDoc<{ createdAt: number; retiredAt: number | null }>({
  kind: LIFECYCLE_KIND, // the purge removes it last (purge.ts)
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ createdAt: 0, retiredAt: null }),
  checkpointWhen: () => true, // it changes at most twice
})

class HttpError extends Error {
  /** reason: a stable word for a refusal people can cause, which the server gives its own error code. */
  constructor(
    readonly status: number,
    message: string,
    readonly reason?: 'unknown_model' | 'no_model',
  ) {
    super(message)
  }
}

const MAX_BODY = 1 << 20

/** How long a purge waits for readers of the database before it says it isn't done. */
const walBusyMs = testHooks.walBusyMs ?? 15_000

async function readJson(req: IncomingMessage): Promise<Record<string, unknown>> {
  const chunks: Buffer[] = []
  let size = 0
  for await (const chunk of req) {
    size += (chunk as Buffer).length // bytes, not characters
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

// Every response body passes redact(): a model error can quote an API key an upstream echoed.
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

// Pi Durable IDs are numbers; they cross the wire as decimal path segments.
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

// The conversations the server created (ownerless: a subagent's belongs to a task), retired ones
// included, oldest first.
async function listConversations() {
  const out: { id: number; createdAt: number; retired: boolean }[] = []
  let cursor: Cursor | undefined
  do {
    // A commit that only reads stores nothing.
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

// Entries a page holds by default and at most: a view's window holds 50 to 100.
const PAGE_ENTRIES = 50
const MAX_PAGE_ENTRIES = 100

// The page of a conversation's entries just before the entry `before` (all of them up to the
// newest without it), oldest first, shaped like the watch's view: `before` is the entry just
// before the page, absent when the page starts the transcript.
async function entries(id: string, query: URLSearchParams) {
  const conv = await conversation(id)
  const before = query.has('before') ? parseId(query.get('before') ?? '') : undefined
  const limit = query.has('limit') ? parseId(query.get('limit') ?? '') : PAGE_ENTRIES
  if (limit < 1 || limit > MAX_PAGE_ENTRIES) throw new HttpError(400, `limit must be 1 to ${MAX_PAGE_ENTRIES}`)
  if (before === 0) return { entries: [] }
  // Newest first, one more than the page, to know the entry before it.
  const page = await conv.entries(
    before === undefined ? {} : { maxEntryId: (before - 1) as EntryId },
    limit + 1,
    undefined,
    context,
  )
  const items = page.items.slice(0, limit).reverse()
  return page.items.length > limit ? { entries: items, before: page.items[limit].id } : { entries: items }
}

// Write one NDJSON line and wait while the client is behind. 'drain' never comes once the
// client has gone, so a close or error ends the wait too.
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
  // The next folded batch waits until this one is flushed.
  handle.start(async (call) => {
    if (call.kind !== 'value') return
    try {
      await writeLine(res, { type: 'ops', ops: call.ops })
    } catch (error) {
      // One broken stream must not take the sidecar down: end it and stop watching.
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
  // /resource/id/action; a missing id is '' and parseId rejects it with 400.
  const [resource, id = '', action] = parts

  // The pid lets the supervisor tell its own sidecar from one a crashed server left behind.
  if (req.method === 'GET' && url.pathname === '/healthz') return send(res, 200, { status: 'ok', pid: process.pid })

  if (req.method === 'GET' && url.pathname === '/models') return send(res, 200, catalog)

  // Re-reads the providers file. Validation and every key read happen before anything changes,
  // so a refused file leaves the current providers in place. Running conversations go on: each
  // model request resolves its provider afresh, so one whose model is gone fails its next
  // request with no_model, and the others pick up a changed endpoint or key from then on.
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
      if (typeof body.content !== 'string') throw new HttpError(400, 'content must be a string')
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
      const conv = await conversation(id)
      const { capability, language } = body as { capability?: string; language?: string }
      if (capability !== undefined || language !== undefined) {
        // Before the input, so the run it starts has them. Rewritten only when they changed.
        await conv.commit(async (tx) => {
          if (capability !== undefined) {
            const access = await tx.doc(ToolAccess, conv.id)
            if (access.capability !== capability) access.capability = capability
          }
          if (language !== undefined) {
            const lang = await tx.doc(Language, conv.id)
            if (lang.tag !== language) lang.tag = language
          }
        }, context)
      }
      const submission = await conv.submit(
        {
          type: 'input',
          content: body.content,
          ...(whenBusy ? { whenBusy } : {}),
          ...(typeof body.requestId === 'string' ? { requestId: body.requestId } : {}),
        },
        context,
      )
      return send(res, 202, { submissionId: submission.id })
    }
    if (req.method === 'POST' && action === 'retire') {
      const found = await conversation(id)
      // Marked first: from then on conversation() refuses it, so no new input can start work
      // after the abort below. A second retire is a 404.
      await found.commit(async (tx) => {
        ;(await tx.doc(Lifecycle, found.id)).retiredAt = Date.now()
      }, context)
      await found.abort(context) // withdraws queued input and stops the running work
      console.error(`conversation ${found.id} retired`)
      res.writeHead(204).end()
      return
    }
    if (req.method === 'POST' && action === 'purge') {
      const num = parseId(id) as ConversationId
      const found = await harness.conversation(num, context)
      if (found) {
        if ((await harness.snapshot(Lifecycle, num, context))?.retiredAt == null) {
          throw new HttpError(409, 'retire the conversation before purging it')
        }
        await found.abort(context) // idle, as retiring left it
        // On a worker thread (purge.ts): its waits for the database never stall other turns.
        await runPurge({ path: dbPath as string, root: num, timeoutMs: walBusyMs })
        console.error(`conversation ${num} purged`)
      } else {
        // Gone already, perhaps by an earlier purge that deleted the rows and couldn't truncate the
        // log: what was said can still be in it, so done means the log is empty too.
        await runPurge({ path: dbPath as string, timeoutMs: walBusyMs })
      }
      res.writeHead(204).end()
      return
    }
    if (req.method === 'POST' && action === 'abort') {
      // Stop: queued input is withdrawn (queued writes stay) and the running work aborted.
      await (await conversation(id)).abort(context)
      res.writeHead(204).end()
      return
    }
    if (req.method === 'GET' && action === 'entries') return send(res, 200, await entries(id, url.searchParams))
    if (req.method === 'GET' && action === 'watch') return watch(id, req, res)
    if (req.method === 'GET' && action === 'model') {
      const agent = await (await conversation(id)).agent(context)
      // Distinct from 404 (unknown conversation): the conversation exists but has no model.
      if (!agent.model) throw new HttpError(409, 'conversation has no model', 'no_model')
      // Only the reference: never the resolved model's base URL or settings.
      return send(res, 200, { provider: agent.model.provider, modelId: agent.model.modelId })
    }
    if (req.method === 'PUT' && action === 'model') {
      const model = modelRef(await readJson(req))
      // Takes effect from the conversation's next model request, also mid-run.
      await (await conversation(id)).configure({ model }, context)
      return send(res, 200, model)
    }
    if (req.method === 'GET' && action === 'messages') {
      // What the model sees next, simplified to role + text.
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
    // A purge that couldn't empty the log is not an error of the sidecar's: it isn't done yet.
    const status = error instanceof HttpError ? error.status : error instanceof WalBusyError ? 503 : 500
    // What went wrong goes to the sidecar's own log; the answer says what a caller may act on: a
    // refusal of ours, or that the purge isn't done, and for anything else only that it failed.
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

rmSync(socketPath, { force: true }) // a stale socket from a crashed process
// No auth here: only the owner (the Go server's user) may connect. The umask makes the
// socket owner-only from the moment it exists; the chmod is a second guard.
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
  server.closeAllConnections() // end open watch streams, which close() alone leaves running
  // Closing waits for running task code; don't let a stuck close keep the process alive.
  const timeout = new Promise<'timeout'>((resolve) => setTimeout(() => resolve('timeout'), 10_000).unref())
  const closed = await Promise.race([harness.close(context).then(() => 'closed' as const), timeout])
  if (closed === 'timeout') console.error('harness did not close within 10 s; exiting anyway')
  process.exit(closed === 'closed' ? 0 : 1)
}
process.once('SIGTERM', () => void shutdown())
process.once('SIGINT', () => void shutdown())
if (process.env.TASKTO_AGENT_EXIT_ON_EOF === '1') {
  // Nothing is ever written to stdin; reading it only tells when the server is gone.
  process.stdin.on('data', () => {})
  process.stdin.once('end', () => {
    console.error('stdin closed: the server is gone; shutting down')
    void shutdown()
  })
  process.stdin.once('error', () => void shutdown())
}
