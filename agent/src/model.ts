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
    tokensPerSecond: 40, // slow enough to see partial answers stream
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
