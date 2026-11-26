// CLI workers (docs/cli-workers.md): a conversation whose model is a CLI (provider kind "cli",
// "cli-claude", "cli-codex", "cli-grok") runs each turn on the official CLI, signed in with the
// conversation owner's own login. The Go server runs the CLI; the sidecar records the turn.
//
// The model of such a conversation is a driver that makes no model call: it turns the user's
// new input into one cli_turn tool call, and the tool's result into the answer. The tool calls
// the server on the tools socket (POST /cli/turn) with the conversation's capability, streams
// the CLI's work into its output as it happens, and returns the CLI's answer.
//
// The tool is replay-safe. It memoises a turn ID before it calls the server, so a sidecar that
// restarts in the middle of a turn runs it again with the same ID, and the server, whose worker
// still has the turn, sends its events from the start instead of running the CLI twice. If the
// worker restarted too, the server resumes the CLI session and says so ("interrupted").
//
// An expired login doesn't fail the turn: the tool says so, waits until the owner signs in again
// (POST /cli/wait, which answers once the account is connected), and goes on with the turn.
//
// The CLI has taskto's tools over MCP (docs/cli-tools.md). When it asks the user to take over
// the browser (browser_help), the request is kept in the result's details, as `help`, so the
// conversation shows the same card as for the built-in agent's call.

import { randomBytes } from 'node:crypto'
import { request } from 'node:http'
import type { createModels } from '@earendil-works/pi-ai/models'
import {
  type FauxResponseFactory,
  fauxAssistantMessage,
  fauxProvider,
  fauxText,
  fauxToolCall,
} from '@earendil-works/pi-ai/providers/faux'
import { defineExtension, defineTool, type ToolExecutionApi } from '@earendil-works/pi-durable'
import { obj, str } from '@earendil-works/pi-schema'
import { ToolAccess } from './taskto-tools.js'

export const CLI_TOOL = 'cli_turn'
export const CLI_NAMES = ['claude', 'codex', 'grok'] as const
export type CliName = (typeof CLI_NAMES)[number]

const PRODUCT: Record<CliName, string> = { claude: 'Claude Code', codex: 'Codex', grok: 'Grok Build' }

export type CliProviderConfig = {
  kind: 'cli'
  id: string
  name: string
  cli: CliName
  models: { id: string; name: string }[]
}

type Message = { role: string; content?: unknown; toolName?: string; details?: unknown }

function textOf(content: unknown): string {
  if (typeof content === 'string') return content
  if (!Array.isArray(content)) return ''
  return content
    .filter((part): part is { type: 'text'; text: string } => part?.type === 'text')
    .map((part) => part.text)
    .join('')
}

/** Registers the driver model of a CLI provider with pi-ai. */
export function installCliDriver(models: ReturnType<typeof createModels>, p: CliProviderConfig): void {
  const faux = fauxProvider({
    provider: p.id,
    models: p.models.map((m) => ({ id: m.id, name: m.name })),
    tokensPerSecond: 4000, // the answer is already there: show it at once
  })
  const answer: FauxResponseFactory = (context, _options, _state, model) => {
    if (faux.getPendingResponseCount() < 1000) refill()
    const last = (context.messages as Message[]).at(-1)
    if (last?.role === 'toolResult' && last.toolName === CLI_TOOL) {
      return fauxAssistantMessage(fauxText(textOf(last.content) || '(the CLI gave no answer)'))
    }
    return fauxAssistantMessage(fauxToolCall(CLI_TOOL, { cli: p.cli, model: model.id }), { stopReason: 'toolUse' })
  }
  // The faux provider takes a list of steps; the factory is stateless, so any number of them
  // answer the same way, and it tops the list up as it goes.
  const refill = () => faux.appendResponses(Array.from({ length: 10_000 }, () => answer))
  faux.setResponses([])
  refill()
  models.setProvider(faux.provider)
}

/** A conversation's messages as the tool needs them. */
export type MessagesOf = (conversationId: number) => Promise<readonly Message[]>

type CliEvent = {
  seq?: number
  type: string
  text?: string
  status?: string
  tool?: { id?: string; name: string; input?: string; status: string; output?: string; saved?: SavedApp }
  permission?: { id: string; tool: string; input?: string; status: string; reason?: string }
}

type Done = { status: string; text: string }

/** An app the CLI saved with taskto's create_app or update_app (the server read it from the whole answer). */
export type SavedApp = { space: number; app: number }

/** A browser_help call's request to the user: why, and what to do. */
type Help = { reason: string; message: string }

/**
 * The CLI's latest browser_help call in a turn. As for the built-in agent's, the card is shown
 * from the call itself: what the model asked the user to do.
 */
class Helps {
  private help?: Help
  // The apps the turn saved, by the call that saved each (a replayed event is the same call).
  private apps = new Map<string, SavedApp>()
  /** Records a tool call event. */
  see(tool: NonNullable<CliEvent['tool']>): void {
    const { saved } = tool
    if (
      saved &&
      Number.isSafeInteger(saved.space) &&
      Number.isSafeInteger(saved.app) &&
      saved.space > 0 &&
      saved.app > 0
    ) {
      this.apps.set(tool.id ?? `${this.apps.size}`, { space: saved.space, app: saved.app })
    }
    if (tool.status !== 'in_progress') return
    this.help = helpOf(tool) ?? this.help
  }
  latest(): Help | undefined {
    return this.help
  }
  /** The apps the turn saved, in the order they were saved. */
  savedApps(): SavedApp[] {
    return [...this.apps.values()]
  }
}

// The names a CLI gives taskto's browser_help: Claude Code's MCP name, the worker's for a Codex
// MCP call (server.tool), and Grok Build's; Grok Build may also call it through use_tool.
const HELP_NAMES = new Set(['mcp__taskto__browser_help', 'taskto.browser_help', 'taskto__browser_help'])

/** The request of a CLI's tool call, if it is taskto's browser_help with a message. */
export function helpOf(tool: { name: string; input?: string }): Help | undefined {
  let args: unknown
  try {
    args = JSON.parse(tool.input ?? '')
  } catch {
    return undefined
  }
  if (tool.name === 'use_tool') {
    const call = args as { tool_name?: unknown; tool_input?: unknown } | null
    if (call?.tool_name !== 'taskto__browser_help') return undefined
    args = call.tool_input
  } else if (!HELP_NAMES.has(tool.name)) {
    return undefined
  }
  const a = args as { reason?: unknown; message?: unknown } | null
  if (typeof a?.message !== 'string' || !a.message.trim()) return undefined
  return { reason: typeof a.reason === 'string' ? a.reason : 'other', message: a.message.trim() }
}

// How long a call waits for the server's first byte, and for its socket while it starts.
const CONNECT_RETRY_MS = 30_000
const WAIT_FOR_LOGIN_MS = 24 * 60 * 60 * 1000
const MAX_HISTORY = 64 << 10

// What the CLI did in a turn, as the tool's output showed it while it ran, is kept in the
// result's details (the output gives way to the result): the conversation shows it after the
// turn too. It is for people, not the model, which reads only the answer. Bounded.
const MAX_TRANSCRIPT = 64 << 10

// How many saved apps a turn's result keeps: the last ones.
const MAX_APPS = 20

function result(value: string, transcript: string, isError = false, help?: Help, apps: SavedApp[] = []) {
  const details = {
    ...(transcript ? { transcript: transcript.slice(-MAX_TRANSCRIPT) } : {}),
    ...(help ? { help } : {}),
    // The apps the CLI saved: the conversation shows them like the built-in agent's saved apps.
    ...(apps.length > 0 ? { apps: apps.slice(-MAX_APPS) } : {}),
  }
  return {
    output: [{ type: 'text' as const, text: value }],
    ...(Object.keys(details).length > 0 ? { details } : {}),
    ...(isError ? { isError: true } : {}),
  }
}

/** api.output, keeping what it shows for the result's details too. */
function recorder(api: ToolExecutionApi): { api: ToolExecutionApi; text: () => string } {
  let text = ''
  const output = (chunk: string) => {
    text = (text + chunk).slice(-MAX_TRANSCRIPT)
    api.output(chunk)
  }
  return { api: { ...api, output } as ToolExecutionApi, text: () => text }
}

/**
 * Splits the conversation at the current turn: the prompt is the user's input since the last
 * CLI turn (or the start), the history everything before it, for a CLI session that starts in
 * the middle of a conversation.
 */
export function splitTurn(messages: readonly Message[]): { prompt: string; history: { role: string; text: string }[] } {
  let end = messages.length
  // The newest entries are this very tool call: an assistant message without text, and the
  // stand-in result the view shows for a call still running.
  const current = (m: Message | undefined) =>
    (m?.role === 'assistant' && !textOf(m.content).trim()) ||
    (m?.role === 'toolResult' && (m.details as { reason?: unknown } | undefined)?.reason === 'missing_result')
  while (end > 0 && current(messages[end - 1])) end--
  let start = end
  while (start > 0 && messages[start - 1]?.role === 'user') start--
  const prompt = messages
    .slice(start, end)
    .map((m) => textOf(m.content).trim())
    .filter(Boolean)
    .join('\n\n')
  const history: { role: string; text: string }[] = []
  let size = 0
  for (let i = start - 1; i >= 0; i--) {
    const m = messages[i]
    if (!m || (m.role !== 'user' && m.role !== 'assistant')) continue
    const text = textOf(m.content).trim()
    if (!text) continue
    if (size + text.length > MAX_HISTORY) break
    size += text.length
    history.unshift({ role: m.role, text })
  }
  return { prompt, history }
}

function newTurnId(): string {
  return randomBytes(16).toString('hex')
}

class HttpRefusal extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message)
  }
}

/** POSTs body to path on the tools socket and calls onLine with each line of the answer. */
function post(
  socketPath: string,
  path: string,
  capability: string,
  body: unknown,
  onLine: (line: string) => void,
  signal?: AbortSignal,
): Promise<void> {
  const payload = JSON.stringify(body)
  return new Promise((resolve, reject) => {
    const req = request(
      {
        socketPath,
        path,
        method: 'POST',
        headers: {
          Authorization: `Bearer ${capability}`,
          'Content-Type': 'application/json',
          'Content-Length': Buffer.byteLength(payload),
        },
        signal,
      },
      (res) => {
        let buffer = ''
        res.setEncoding('utf8')
        if ((res.statusCode ?? 0) >= 300) {
          res.on('data', (chunk: string) => {
            buffer = (buffer + chunk).slice(0, 4096)
          })
          res.on('end', () => {
            let message = `the server answered ${res.statusCode}`
            try {
              const parsed = JSON.parse(buffer) as { error?: unknown }
              if (typeof parsed.error === 'string') message = parsed.error
            } catch {
              // not JSON
            }
            reject(new HttpRefusal(res.statusCode ?? 0, message))
          })
          return
        }
        res.on('data', (chunk: string) => {
          buffer += chunk
          let nl = buffer.indexOf('\n')
          while (nl >= 0) {
            const line = buffer.slice(0, nl)
            buffer = buffer.slice(nl + 1)
            if (line.trim()) onLine(line)
            nl = buffer.indexOf('\n')
          }
        })
        res.on('end', () => {
          if (buffer.trim()) onLine(buffer)
          resolve()
        })
        res.on('error', reject)
      },
    )
    req.on('error', reject)
    req.end(payload)
  })
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

function unreachable(error: unknown): boolean {
  const code = (error as NodeJS.ErrnoException).code
  return code === 'ENOENT' || code === 'ECONNREFUSED' || code === 'ECONNRESET' || code === 'EPIPE'
}

// One line per event, readable as it streams: the CLI's text as it comes, its tool calls, and
// what the server says. Thinking and token counts stay out. Tool calls go to helps too.
function show(api: ToolExecutionApi, cli: CliName, e: CliEvent, helps: Helps): void {
  switch (e.type) {
    case 'text':
      if (e.text) api.output(`${e.text}\n`)
      break
    case 'tool':
      if (!e.tool) break
      helps.see(e.tool)
      if (e.tool.status === 'in_progress') {
        api.output(`▸ ${e.tool.name}${e.tool.input ? ` ${e.tool.input.slice(0, 300)}` : ''}\n`)
      } else if (e.tool.status === 'failed') {
        api.output(`  ✗ ${e.tool.name}${e.tool.output ? `: ${e.tool.output.slice(0, 300)}` : ''}\n`)
      }
      break
    case 'notice':
      if (e.text) api.output(`· ${e.text}\n`)
      break
    case 'interrupted':
      api.output(`⟲ ${e.text ?? 'interrupted by a restart; resuming'}\n`)
      break
    case 'login_required':
      api.output(`⚠ ${PRODUCT[cli]} needs you to sign in again.\n`)
      break
    // A permission question to the owner, answered in the web app (docs/agent-safety.md),
    // and later its answer.
    case 'permission': {
      const p = e.permission
      if (!p) break
      if (p.status === 'asked') {
        api.output(`? ${PRODUCT[cli]} asks to use ${p.tool}${p.input ? `: ${p.input.slice(0, 300)}` : ''}\n`)
      } else if (p.status === 'allowed') {
        api.output(`  ✓ allowed\n`)
      } else {
        api.output(`  ✗ not allowed${p.reason ? ` (${p.reason})` : ''}\n`)
      }
      break
    }
  }
}

/** The cli_turn tool, calling the server on socketPath. */
export function cliTools(socketPath: string, messagesOf: MessagesOf) {
  // Runs (or re-attaches to) one worker turn and returns how it ended.
  async function runTurn(
    api: ToolExecutionApi,
    capability: string,
    body: Record<string, unknown>,
    cli: CliName,
    helps: Helps,
    signal?: AbortSignal,
  ): Promise<Done> {
    // How long the server has been failing us: since the first failure after the last line
    // that came through, so a long turn that loses its connection late still re-attaches.
    let failingSince: number | undefined
    const givenUp = () => failingSince !== undefined && Date.now() - failingSince > CONNECT_RETRY_MS
    let seen = 0
    for (;;) {
      let done: Done | undefined
      try {
        await post(
          socketPath,
          '/cli/turn',
          capability,
          body,
          (line) => {
            failingSince = undefined
            let e: CliEvent
            try {
              e = JSON.parse(line) as CliEvent
            } catch {
              return
            }
            // A resumed turn is a new worker turn: its events count from 1 again.
            if (e.type === 'interrupted') seen = 0
            // Re-attached after a lost connection, the server sends the turn from its start.
            if (typeof e.seq === 'number' && e.seq > 0) {
              if (e.seq <= seen) return
              seen = e.seq
            }
            if (e.type === 'done') {
              done = { status: e.status ?? 'failed', text: e.text ?? '' }
              return
            }
            show(api, cli, e, helps)
          },
          signal,
        )
      } catch (error) {
        if (signal?.aborted) throw error
        if (error instanceof HttpRefusal) return { status: 'failed', text: error.message }
        failingSince ??= Date.now()
        if (!unreachable(error) || givenUp()) {
          console.error(`${CLI_TOOL}: the server is unreachable:`, (error as Error).message)
          return { status: 'failed', text: 'the server is unreachable right now: try again shortly' }
        }
        await sleep(500)
        continue
      }
      if (done) return done
      // The stream ended without its last event (the server restarted): read the turn again,
      // as a replay, so a turn the worker lost is resumed rather than run as new.
      failingSince ??= Date.now()
      if (givenUp()) return { status: 'failed', text: 'the CLI turn ended without an answer' }
      body = { ...body, replay: true }
      await sleep(500)
    }
  }

  // Waits until the owner's account for cli is connected again: true, or false after a day.
  async function waitForLogin(capability: string, cli: CliName, signal?: AbortSignal): Promise<boolean> {
    const until = Date.now() + WAIT_FOR_LOGIN_MS
    while (Date.now() < until) {
      let state = ''
      try {
        await post(
          socketPath,
          '/cli/wait',
          capability,
          { cli },
          (line) => {
            try {
              state = String((JSON.parse(line) as { state?: unknown }).state ?? '')
            } catch {
              // not JSON
            }
          },
          signal,
        )
      } catch (error) {
        if (signal?.aborted) throw error
        // A refusal (the conversation archived, its capability changed) won't pass by itself.
        if (error instanceof HttpRefusal) throw error
        await sleep(5_000)
        continue
      }
      if (state === 'connected') return true
    }
    return false
  }

  const tool = defineTool({
    name: CLI_TOOL,
    description: "Runs the user's input on their CLI worker (Claude Code, Codex or Grok Build)",
    parameters: obj({ cli: str(), model: str() }),
    replay: 'safe',
    execute: async (args, toolApi, context) => {
      const rec = recorder(toolApi)
      const api = rec.api
      if (!(CLI_NAMES as readonly string[]).includes(args.cli)) return result(`unknown CLI ${args.cli}`, '', true)
      const cli = args.cli as CliName
      const access = await api.snapshot(ToolAccess, api.conversationId, context)
      if (!access?.capability) {
        return result(
          'the CLI worker is not available in this conversation yet: it will be from the next message',
          '',
          true,
        )
      }
      const signal = context.abortSignal
      // Each run of this call memoises its number: a later one is a replay after a restart.
      let attempt = 1
      while ((await api.memo(`attempt-${attempt}`, context)) !== undefined) attempt++
      await api.memo(`attempt-${attempt}`, Date.now(), context)
      let replay = attempt > 1
      // A turn that waited for a new login goes on as another worker turn: resume the newest.
      let round = 1
      while ((await api.memo(`turn-${round + 1}`, context)) !== undefined) round++

      const { prompt, history } = splitTurn(await messagesOf(api.conversationId))
      if (!prompt) return result('there is no new message for the CLI', '', true)
      // The CLI's requests to take over the browser, for the conversation's card.
      const helps = new Helps()
      for (;;) {
        const turn = await api.memo(`turn-${round}`, newTurnId(), context)
        let done: Done
        try {
          done = await runTurn(
            api,
            access.capability,
            { turn, cli, model: args.model, prompt, history, replay },
            cli,
            helps,
            signal,
          )
        } catch (error) {
          if (signal?.aborted) {
            // Stopped: stop the CLI too. Not awaited by anyone, so it can't hold the abort up.
            void post(socketPath, '/cli/stop', access.capability, { turn }, () => {}).catch(() => {})
          }
          throw error
        }
        replay = false
        if (done.status === 'login_required') {
          api.output(
            `⚠ ${PRODUCT[cli]} needs you to sign in again: reconnect it (Settings → CLI accounts). This conversation goes on by itself once you have.\n`,
          )
          let connected: boolean
          try {
            connected = await waitForLogin(access.capability, cli, signal)
          } catch (error) {
            if (error instanceof HttpRefusal) return result(error.message, rec.text(), true)
            throw error
          }
          if (!connected) {
            return result(
              `${PRODUCT[cli]} wasn't signed in again within a day: sign in, then send your message again`,
              rec.text(),
              true,
            )
          }
          api.output(`✓ ${PRODUCT[cli]} is signed in again: continuing.\n`)
          round++
          continue
        }
        if (done.status === 'ok') return result(done.text, rec.text(), false, helps.latest(), helps.savedApps())
        return result(
          done.text || `the ${PRODUCT[cli]} turn failed`,
          rec.text(),
          true,
          helps.latest(),
          helps.savedApps(),
        )
      }
    },
  })
  return defineExtension({ name: 'cli-workers', tools: [tool] })
}
