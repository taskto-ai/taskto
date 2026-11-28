import type { ModelRef } from '../api'
import type { ModelProvider } from './ModelPicker'

export type AgentChoice = { ref: ModelRef; label: string; menu: string; cli: boolean }

const PROVIDER = /^[A-Za-z0-9._-]{1,256}$/
const MODEL = /^[A-Za-z0-9._:-]{1,256}$/
const LEADING = /^@\[([^\][\\\n]{1,100})\]\(agent:([A-Za-z0-9._-]{1,256})\/([A-Za-z0-9._:-]{1,256})\)(?:[\t\n\f\r ]+|$)/

export function agentLabel(text: string): string {
  return Array.from(text.replace(/[\][\\\n\r]/g, ' '))
    .slice(0, 100)
    .join('')
    .trim()
}

export function agentToken(choice: AgentChoice): string {
  return `@[${choice.label}](agent:${choice.ref.provider}/${choice.ref.model_id})`
}

export function leadingAgent(text: string): { label: string; ref: ModelRef; rest: string } | null {
  const m = LEADING.exec(text)
  if (!m) return null
  return { label: m[1], ref: { provider: m[2], model_id: m[3] }, rest: text.slice(m[0].length) }
}

export function agentChoices(
  providers: ModelProvider[],
  menu: (provider: string, model: string) => string,
): AgentChoice[] {
  const choices: AgentChoice[] = []
  for (const p of providers) {
    const cli = p.scope === 'cli'
    for (const m of p.models) {
      if (!PROVIDER.test(p.provider) || !MODEL.test(m.id)) continue
      const label = agentLabel(cli ? (m.id === 'default' ? p.name : `${p.name}: ${m.name}`) : m.name)
      if (!label) continue
      choices.push({
        ref: { provider: p.provider, model_id: m.id },
        label,
        menu: p.name === m.name ? m.name : menu(p.name, m.name),
        cli,
      })
    }
  }
  return choices.sort((a, b) => Number(b.cli) - Number(a.cli))
}

export function mentionQuery(text: string): string | null {
  return /^@[^\s[\]]*$/.test(text) ? text.slice(1).toLowerCase() : null
}

export function matchingAgents(choices: AgentChoice[], query: string): AgentChoice[] {
  return choices.filter((c) => c.label.toLowerCase().includes(query) || c.menu.toLowerCase().includes(query))
}
