import { type CliName, isCliName } from '../api'
import { t } from '../i18n'
import { type Changed, changeOf, isId } from './changes'

export type ConversationView = {
  entries?: { id: number; kind: string; model?: unknown[]; data?: unknown }[]
  before?: number
  docs?: Record<string, unknown>
}

export type AppRef = { space: number; app: number }

export type ToolUse = {
  id: string
  key: string
  name: string
  app?: AppRef
  cli?: CliName
  changed?: Changed
  output?: string
  running?: boolean
  help?: { reason: string; message: string }
}

export const NOTE_KIND = 'taskto.note'

export const HELP_TOOL = 'browser_help'

export const CLI_TOOL = 'cli_turn'

const APP_TOOLS = new Set(['create_app', 'update_app'])

export type ChatMessage = {
  key: string
  role: 'user' | 'assistant' | 'note'
  text: string
  tools: ToolUse[]
  author?: number
  error?: string
  failed?: boolean
  pending?: boolean
}

export type Transcript = {
  messages: ChatMessage[]
  lastEntry?: number
  running: boolean
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

function callsOf(content: unknown, key: string): { part: Part; at: string }[] {
  if (!Array.isArray(content)) return []
  return (content as Part[])
    .map((part, j) => ({ part, at: `${key}.${j}` }))
    .filter(({ part }) => part?.type === 'toolCall' && typeof part.name === 'string')
}

type CliKept = { transcript?: string; help?: ToolUse['help']; apps?: AppRef[] }

type Kept = { saved: Map<string, AppRef>; done: Map<string, CliKept>; changed: Map<string, Changed> }

function toolsOf(content: unknown, key: string, { saved, done, changed }: Kept): ToolUse[] {
  return callsOf(content, key).map(({ part, at }) => {
    const use: ToolUse = { id: String(part.id ?? part.name), key: at, name: part.name as string }
    const app = APP_TOOLS.has(use.name) ? saved.get(at) : undefined
    if (app) use.app = app
    const change = changed.get(at)
    if (change) use.changed = change
    const cli = use.name === CLI_TOOL ? (part.arguments as { cli?: unknown } | undefined)?.cli : undefined
    if (isCliName(cli)) use.cli = cli
    if (use.name === HELP_TOOL) {
      const args = part.arguments as { reason?: unknown; message?: unknown } | undefined
      if (typeof args?.message === 'string' && args.message.trim()) {
        use.help = { reason: typeof args.reason === 'string' ? args.reason : 'other', message: args.message.trim() }
      }
    }
    const kept = use.name === CLI_TOOL ? done.get(at) : undefined
    if (kept?.transcript) use.output = kept.transcript
    if (kept?.help) use.help = kept.help
    const apps = kept?.apps
    if (apps?.length) use.app = apps[apps.length - 1]
    return use
  })
}

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
  const apps = (Array.isArray(m.details?.apps) ? m.details.apps : []).flatMap(
    (a: { space?: unknown; app?: unknown }) => (isId(a?.space) && isId(a.app) ? [{ space: a.space, app: a.app }] : []),
  )
  if (apps.length > 0) kept.apps = apps
  return kept.transcript || kept.help || kept.apps ? kept : undefined
}

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

function toMessage(key: string, message: unknown, kept: Kept, pending = false): ChatMessage | null {
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
  const out: ChatMessage = { key, role: m.role, text: textOf(m.content), tools: toolsOf(m.content, key, kept) }
  if (error) out.error = error
  if (m.role === 'assistant' && m.stopReason === 'error') out.failed = true
  if (pending) out.pending = true
  if (!out.text && out.tools.length === 0 && !out.error && !pending) return null
  return out
}

function toNote(entry: { id: number; kind: string; data?: unknown }): ChatMessage | null {
  if (entry.kind !== NOTE_KIND) return null
  const data = entry.data as { text?: unknown; author?: unknown } | undefined
  if (typeof data?.text !== 'string' || !data.text.trim()) return null
  const note: ChatMessage = { key: `${entry.id}`, role: 'note', text: data.text, tools: [] }
  if (isId(data.author)) note.author = data.author
  return note
}

type Slot = { callId?: unknown; name?: unknown; status?: unknown; output?: unknown }

type Call = { at: string; name: string; args: unknown }

function changeOfResult(call: Call | undefined, message: unknown): Changed | undefined {
  const m = message as { isError?: unknown; content?: unknown }
  return call && !m.isError ? changeOf(call.name, call.args, textOf(m.content)) : undefined
}

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
  const entries = (view?.entries ?? []).filter((e) => Array.isArray(e?.model) || e?.kind === NOTE_KIND)
  const saved = new Map<string, AppRef>()
  const done = new Map<string, CliKept>()
  const changed = new Map<string, Changed>()
  const open = new Map<string, Call>()
  for (const entry of entries) {
    entry.model?.forEach((message, i) => {
      for (const { part, at } of callsOf((message as { content?: unknown })?.content, `${entry.id}.${i}`)) {
        open.set(String(part.id ?? part.name), { at, name: part.name as string, args: part.arguments })
      }
      const id =
        (message as { role?: unknown; toolCallId?: unknown })?.role === 'toolResult'
          ? (message as { toolCallId?: unknown }).toolCallId
          : undefined
      if (typeof id !== 'string') return
      const call = open.get(id)
      const at = call?.at
      open.delete(id)
      const app = savedApp(message)
      if (at && app) saved.set(at, app[1])
      const kept = cliKept(message)
      if (at && kept) done.set(at, kept)
      const change = changeOfResult(call, message)
      if (at && change) changed.set(at, change)
    })
  }
  const results: Kept = { saved, done, changed }
  for (const entry of entries) {
    const note = toNote(entry)
    if (note) messages.push(note)
    entry.model?.forEach((message, i) => {
      const m = toMessage(`${entry.id}.${i}`, message, results)
      if (m) messages.push(m)
    })
  }
  const live = view?.docs?.['pi.live'] as
    | { run?: unknown; generation?: { message?: unknown }; tools?: unknown }
    | undefined
  const partial = live?.generation?.message
  if (partial) {
    const m = toMessage('live', partial, results, true)
    if (m) messages.push(m)
  }
  liveOutput(messages, live?.tools)
  const lastEntry = view?.entries?.at(-1)?.id
  return {
    messages,
    ...(lastEntry === undefined ? {} : { lastEntry }),
    running: Boolean(live?.run),
    truncated: view?.before !== undefined,
  }
}

export function pendingHelp(messages: ChatMessage[]): ToolUse['help'] | null {
  let help: ToolUse['help'] | null = null
  for (const m of messages) {
    if (m.role === 'note') continue
    if (m.role === 'user') help = null
    else for (const t of m.tools) if (t.help) help = t.help
  }
  return help
}
