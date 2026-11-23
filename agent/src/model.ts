// A scripted offline model for development and the spike tests: no keys, no network,
// and the same answer for the same context, also after a restart.
//
//   "tool:<name>:<ms>"  → the model calls tool <name> with { ms }
//   "tool:<name> <json>" → the model calls tool <name> with the JSON object as its arguments
//   after tool results  → "finished <tool>: <result text>"
//   "system"            → "system: <the system prompt's sections, as they stand>"
//   anything else       → "echo: <text>"
import type { createModels } from '@earendil-works/pi-ai/models'
import { fauxAssistantMessage, fauxProvider, fauxText, fauxToolCall } from '@earendil-works/pi-ai/providers/faux'

export const SCRIPT_MODEL_ID = 'script-1'

// How fast the model writes, in tokens per second: slow enough to see partial answers stream.
// Every answer to a tool's result repeats the result, so a test that only checks what the tools
// returned (a large one, such as the app catalog, takes minutes at this pace) sets this to 0, which
// is no pacing at all, in the environment it starts the sidecar with.
export const TOKENS_PER_SECOND_ENV = 'TASKTO_AGENT_SCRIPT_TOKENS_PER_SECOND'
const DEFAULT_TOKENS_PER_SECOND = 40

export function tokensPerSecond(env: NodeJS.ProcessEnv = process.env): number {
  const set = env[TOKENS_PER_SECOND_ENV]?.trim()
  if (!set) return DEFAULT_TOKENS_PER_SECOND
  const value = Number(set)
  return Number.isFinite(value) && value >= 0 ? value : DEFAULT_TOKENS_PER_SECOND
}

type Message = { role: string; content?: unknown; toolName?: string; sections?: Record<string, string> }

function textOf(content: unknown): string {
  if (typeof content === 'string') return content
  if (!Array.isArray(content)) return ''
  return content
    .filter((part): part is { type: 'text'; text: string } => part?.type === 'text')
    .map((part) => part.text)
    .join('')
}

export function installScriptModel(models: ReturnType<typeof createModels>, provider: string): void {
  const faux = fauxProvider({
    provider,
    models: [{ id: SCRIPT_MODEL_ID }],
    tokensPerSecond: tokensPerSecond(),
  })
  const answer = (context: { messages: Message[] }) => {
    // Pi Durable puts the system prompt's sections in positional system messages, a changed
    // one after the input that changed it: the instruction is the last other message.
    const last = context.messages.findLast((m) => m.role !== 'system')
    if (last?.role === 'toolResult') {
      return fauxAssistantMessage(fauxText(`finished ${last.toolName}: ${textOf(last.content)}`))
    }
    const text = textOf(last?.content)
    if (text.trim() === 'system') {
      const sections: Record<string, string> = {}
      for (const m of context.messages) if (m.role === 'system') Object.assign(sections, m.sections)
      return fauxAssistantMessage(fauxText(`system: ${Object.values(sections).join('\n')}`))
    }
    const call = /^tool:(?<name>[a-z_]+):(?<ms>\d+)$/.exec(text.trim())?.groups
    if (call?.name && call.ms) {
      return fauxAssistantMessage(fauxToolCall(call.name, { ms: Number(call.ms) }), { stopReason: 'toolUse' })
    }
    const withArgs = /^tool:(?<name>[a-z_]+) (?<args>\{.*\})$/s.exec(text.trim())?.groups
    if (withArgs?.name && withArgs.args) {
      let args: Parameters<typeof fauxToolCall>[1]
      try {
        args = JSON.parse(withArgs.args)
      } catch {
        return fauxAssistantMessage(fauxText(`bad tool arguments: ${withArgs.args}`))
      }
      return fauxAssistantMessage(fauxToolCall(withArgs.name, args), { stopReason: 'toolUse' })
    }
    return fauxAssistantMessage(fauxText(`echo: ${text}`))
  }
  // A factory per request keeps the script stateless, so a restarted process answers the same way.
  // The faux provider takes a finite list, so one process answers at most 10,000 requests.
  const steps = Array.from({ length: 10_000 }, () => answer)
  faux.setResponses(steps as never)
  models.setProvider(faux.provider)
}
