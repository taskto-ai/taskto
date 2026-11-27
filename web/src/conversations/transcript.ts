import { type CliName, isCliName } from '../api'
import { t } from '../i18n'

export type ConversationView = {
  entries?: { id: number; kind: string; model?: unknown[] }[]
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
  output?: string
  running?: boolean
  help?: { reason: string; message: string }
}

export const HELP_TOOL = 'browser_help'

export const CLI_TOOL = 'cli_turn'

const APP_TOOLS = new Set(['create_app', 'update_app'])

export type ChatMessage = {
  key: string
  role: 'user' | 'assistant'
  text: string
  tools: ToolUse[]
  error?: string
  failed?: boolean
  pending?: boolean
}

export type Transcript = {
  messages: ChatMessage[]
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

function toolsOf(content: unknown, key: string, saved: Map<string, AppRef>, done: Map<string, CliKept>): ToolUse[] {
  return callsOf(content, key).map(({ part, at }) => {
    const use: ToolUse = { id: String(part.id ?? part.name), key: at, name: part.name as string }
    const app = APP_TOOLS.has(use.name) ? saved.get(at) : undefined
    if (app) use.app = app
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

const isId = (v: unknown): v is number => typeof v === 'number' && Number.isSafeInteger(v) && v > 0

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
  if (m.role === 'assistant' && m.stopReason === 'error') out.failed = true
  if (pending) out.pending = true
  if (!out.text && out.tools.length === 0 && !out.error && !pending) return null
  return out
}

type Slot = { callId?: unknown; name?: unknown; status?: unknown; output?: unknown }

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
  const saved = new Map<string, AppRef>()
  const done = new Map<string, CliKept>()
  const open = new Map<string, string>()
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

export function pendingHelp(messages: ChatMessage[]): ToolUse['help'] | null {
  let help: ToolUse['help'] | null = null
  for (const m of messages) {
    if (m.role === 'user') help = null
    else for (const t of m.tools) if (t.help) help = t.help
  }
  return help
}
