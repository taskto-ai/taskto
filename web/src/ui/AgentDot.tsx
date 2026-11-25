// The agent's dot (docs/design.md, "How the mint is used"): the website's node, mint ringed with the
// mint edge. It marks what the agent did, beside words that say so, so it is hidden from screen
// readers unless it stands alone with a `label`. `ringed` rings it in ink, for a dot that must be
// seen on its own (an unread mark).

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
