// What the chat shows, from the conversation's view as the events stream keeps it (Pi
// Durable's ConversationView, passed on by the server unchanged): the user's and the agent's
// messages from the view's entries, and the reply being written from its live document.
// Anything of a shape this app doesn't know is passed over rather than drawn wrong.
//
// A tool use that saved an app names the app, from the server's answer to the call: only its
// id and space, so the chat can launch it from the server. What the model sent as such a call's
// arguments is never read: the screen shown is the one the server checked and saved.
//
// A browser_help call's arguments (why, and what the user should do) are read to show the
// user the agent's request, as plain text.
//
// A turn on a CLI worker is one cli_turn tool call: it names the CLI (its one argument the chat
// reads, to say which CLI worked), and while it runs, its output in the live document is the
// CLI's work as plain text lines. When the CLI asked the user to take over the browser (its own
// browser_help call), the turn's result keeps the request, read as the built-in agent's is.

import { type CliName, isCliName } from '../api'
import { t } from '../i18n'

export type ConversationView = {
  entries?: { id: number; kind: string; model?: unknown[] }[]
  before?: number
  docs?: Record<string, unknown>
}

// The app a tool use saved: its space and its id.
export type AppRef = { space: number; app: number }

// cli and output are a cli_turn call's: which CLI, and its running output while it runs. help is a
// browser_help call's: why the agent asks the user to take over the browser, and what to do.
export type ToolUse = {
  id: string
  // Where the call is (its message and its place in it): unlike id, which some providers number
  // from 0 in every turn, it names one call in the whole conversation.
  key: string
  name: string
  app?: AppRef
  cli?: CliName
  output?: string
  running?: boolean
  help?: { reason: string; message: string }
}

export const HELP_TOOL = 'browser_help'

export const CLI_TOOL = 'cli_turn'

// The tools that save an app, answering with it.
const APP_TOOLS = new Set(['create_app', 'update_app'])

export type ChatMessage = {
  key: string
  role: 'user' | 'assistant'
  text: string
  tools: ToolUse[]
  error?: string
  pending?: boolean
}

export type Transcript = {
  messages: ChatMessage[]
  // The agent is working on a submission.
  running: boolean
  // The view starts after the conversation's first entries.
  truncated: boolean
}

type Part = { type?: string; text?: string; id?: string; name?: string; arguments?: unknown }

function textOf(content: unknown): string {
  if (typeof content === 'string') return content
  if (!Array.isArray(content)) return ''
  return (content as Part[])
    .filter((p) => p?.type === 'text' && typeof p.text === 'string')
    .map((p) => p.text)
    .join('')
}

// A message's tool calls, each keyed by where it is (the message's key and its place among
// the message's parts), which no other call shares even if their ids do.
function callsOf(content: unknown, key: string): { part: Part; at: string }[] {
  if (!Array.isArray(content)) return []
  return (content as Part[])
    .map((part, j) => ({ part, at: `${key}.${j}` }))
    .filter(({ part }) => part?.type === 'toolCall' && typeof part.name === 'string')
}

// What a finished cli_turn kept in its result's details: its transcript, and the CLI's request to
// take over the browser.
type CliKept = { transcript?: string; help?: ToolUse['help']; apps?: AppRef[] }

function toolsOf(content: unknown, key: string, saved: Map<string, AppRef>, done: Map<string, CliKept>): ToolUse[] {
  return callsOf(content, key).map(({ part, at }) => {
    const use: ToolUse = { id: String(part.id ?? part.name), key: at, name: part.name as string }
    const app = APP_TOOLS.has(use.name) ? saved.get(at) : undefined
    if (app) use.app = app
    const cli = use.name === CLI_TOOL ? (part.arguments as { cli?: unknown } | undefined)?.cli : undefined
    if (isCliName(cli)) use.cli = cli
    // The agent's request to take over the browser: shown as plain text, never as markup.
    if (use.name === HELP_TOOL) {
      const args = part.arguments as { reason?: unknown; message?: unknown } | undefined
      if (typeof args?.message === 'string' && args.message.trim()) {
        use.help = { reason: typeof args.reason === 'string' ? args.reason : 'other', message: args.message.trim() }
      }
    }
    // A finished CLI turn keeps what it showed while it ran, in its result's details, and the
    // CLI's request to take over the browser.
    const kept = use.name === CLI_TOOL ? done.get(at) : undefined
    if (kept?.transcript) use.output = kept.transcript
    if (kept?.help) use.help = kept.help
    // The app a CLI turn saved last (its MCP calls of create_app and update_app).
    const apps = kept?.apps
    if (apps?.length) use.app = apps[apps.length - 1]
    return use
  })
}

// What a finished cli_turn kept in its result's details (agent/src/cli.ts): the transcript, a
// browser_help request of the CLI's, shown as plain text like the built-in agent's, and the apps
// it saved through taskto's create_app and update_app, which it names like those calls' results do.
function cliKept(message: unknown): CliKept | undefined {
  const m = message as {
    role?: unknown
    toolName?: unknown
    details?: { transcript?: unknown; help?: unknown; apps?: unknown }
  }
  if (m?.role !== 'toolResult' || m.toolName !== CLI_TOOL) return undefined
  const kept: CliKept = {}
  if (typeof m.details?.transcript === 'string' && m.details.transcript) kept.transcript = m.details.transcript
  const help = m.details?.help as { reason?: unknown; message?: unknown } | undefined
  if (typeof help?.message === 'string' && help.message.trim()) {
    kept.help = { reason: typeof help.reason === 'string' ? help.reason : 'other', message: help.message.trim() }
  }
  // The apps the CLI saved in the turn, as the server read them from the answers of its calls.
  const apps = (Array.isArray(m.details?.apps) ? m.details.apps : []).flatMap(
    (a: { space?: unknown; app?: unknown }) => (isId(a?.space) && isId(a.app) ? [{ space: a.space, app: a.app }] : []),
  )
  if (apps.length > 0) kept.apps = apps
  return kept.transcript || kept.help || kept.apps ? kept : undefined
}

const isId = (v: unknown): v is number => typeof v === 'number' && Number.isSafeInteger(v) && v > 0

// The app a tool result saved: a create_app or update_app call that succeeded, answered with
// the app. A refusal (isError, "error: …") or anything else is no app.
function savedApp(message: unknown): [string, AppRef] | null {
  const m = message as { role?: unknown; toolCallId?: unknown; toolName?: unknown; isError?: unknown }
  if (m?.role !== 'toolResult' || m.isError || typeof m.toolCallId !== 'string') return null
  if (typeof m.toolName !== 'string' || !APP_TOOLS.has(m.toolName)) return null
  try {
    const app = JSON.parse(textOf((message as { content?: unknown }).content)) as { id?: unknown; space_id?: unknown }
    return isId(app?.id) && isId(app.space_id) ? [m.toolCallId, { space: app.space_id, app: app.id }] : null
  } catch {
    return null
  }
}

function toMessage(
  key: string,
  message: unknown,
  saved: Map<string, AppRef>,
  done: Map<string, CliKept>,
  pending = false,
): ChatMessage | null {
  if (!message || typeof message !== 'object') return null
  const m = message as { role?: unknown; content?: unknown; stopReason?: unknown; errorMessage?: unknown }
  if (m.role !== 'user' && m.role !== 'assistant') return null
  const error =
    m.role === 'assistant' && (m.stopReason === 'error' || m.stopReason === 'aborted')
      ? typeof m.errorMessage === 'string' && m.errorMessage
        ? m.errorMessage
        : m.stopReason === 'aborted'
          ? t('conversations.replyStopped')
          : t('conversations.replyFailed')
      : undefined
  const out: ChatMessage = { key, role: m.role, text: textOf(m.content), tools: toolsOf(m.content, key, saved, done) }
  if (error) out.error = error
  if (pending) out.pending = true
  // An assistant message that only called tools still shows, as its tools.
  if (!out.text && out.tools.length === 0 && !out.error && !pending) return null
  return out
}

type Slot = { callId?: unknown; name?: unknown; status?: unknown; output?: unknown }

// The running output of the round's cli_turn calls, on the newest use of each call's id.
function liveOutput(messages: ChatMessage[], slots: unknown) {
  if (!Array.isArray(slots)) return
  for (const slot of slots as Slot[]) {
    if (slot?.name !== CLI_TOOL || typeof slot.callId !== 'string') continue
    const use = messages.flatMap((m) => m.tools).findLast((t) => t.id === slot.callId && t.name === CLI_TOOL)
    if (!use) continue
    use.running = slot.status !== 'done'
    if (typeof slot.output === 'string' && slot.output) use.output = slot.output
  }
}

export function transcript(view: ConversationView | null | undefined): Transcript {
  const messages: ChatMessage[] = []
  const entries = (view?.entries ?? []).filter((e) => Array.isArray(e?.model))
  // A call's result comes after it, in the same entry or a later one, and answers the latest
  // call with its id still open: ids aren't unique across a conversation with every provider
  // (some number a turn's calls from 0), so a result is matched by order, not by id alone.
  const saved = new Map<string, AppRef>() // a call's place → the app its result saved
  const done = new Map<string, CliKept>() // a cli_turn call's place → what its result kept
  const open = new Map<string, string>() // a call's id → the place of the latest one unanswered
  for (const entry of entries) {
    entry.model?.forEach((message, i) => {
      for (const { part, at } of callsOf((message as { content?: unknown })?.content, `${entry.id}.${i}`)) {
        open.set(String(part.id ?? part.name), at)
      }
      const id =
        (message as { role?: unknown; toolCallId?: unknown })?.role === 'toolResult'
          ? (message as { toolCallId?: unknown }).toolCallId
          : undefined
      if (typeof id !== 'string') return
      const at = open.get(id)
      open.delete(id)
      const app = savedApp(message)
      if (at && app) saved.set(at, app[1])
      const kept = cliKept(message)
      if (at && kept) done.set(at, kept)
    })
  }
  for (const entry of entries) {
    entry.model?.forEach((message, i) => {
      const m = toMessage(`${entry.id}.${i}`, message, saved, done)
      if (m) messages.push(m)
    })
  }
  const live = view?.docs?.['pi.live'] as
    | { run?: unknown; generation?: { message?: unknown }; tools?: unknown }
    | undefined
  const partial = live?.generation?.message
  if (partial) {
    const m = toMessage('live', partial, saved, done, true)
    if (m) messages.push(m)
  }
  liveOutput(messages, live?.tools)
  return { messages, running: Boolean(live?.run), truncated: view?.before !== undefined }
}

// pendingHelp is the agent's latest request to take over the browser that the user hasn't
// answered: a browser_help call after the user's last message.
export function pendingHelp(messages: ChatMessage[]): ToolUse['help'] | null {
  let help: ToolUse['help'] | null = null
  for (const m of messages) {
    if (m.role === 'user') help = null
    else for (const t of m.tools) if (t.help) help = t.help
  }
  return help
}
