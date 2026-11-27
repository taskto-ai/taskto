import './AgentDot.css'

export interface AgentDotProps {
  label?: string
  ringed?: boolean
  className?: string
}

export function AgentDot({ label, ringed = false, className }: AgentDotProps) {
  const cls = ['ui-agent-dot', ringed ? 'ui-agent-dot-ringed' : '', className ?? ''].filter(Boolean).join(' ')
  const named = label ? { role: 'img', 'aria-label': label } : { 'aria-hidden': true as const }
  return <span className={cls} {...named} />
}
