// One entry of the transcript: the person's message or the agent's reply, with the tools the agent
// used in it, and the error it ended on.

import { useT } from '../i18n'
import { Markdown } from '../markdown'
import { ToolLine } from './ToolLine'
import type { ChatMessage, ToolUse } from './transcript'

export function Message({
  message,
  workspace,
  apps,
}: {
  message: ChatMessage
  workspace: number
  // The tool uses whose app shows launched; the others link to theirs.
  apps: Set<ToolUse>
}) {
  const t = useT()
  return (
    <article className="tt-message" data-role={message.role} aria-busy={message.pending || undefined}>
      <span className="tt-sr-only">{message.role === 'user' ? t('conversations.you') : t('conversations.agent')}</span>
      {message.text &&
        (message.role === 'assistant' ? <AgentText text={message.text} /> : <UserText text={message.text} />)}
      {message.tools.map((tool) => (
        <ToolLine key={tool.id} tool={tool} workspace={workspace} launched={apps.has(tool)} />
      ))}
      {message.pending && !message.text && <span className="tt-muted">{'…'}</span>}
      {message.error && <EntryError error={message.error} />}
    </article>
  )
}

// What the user typed shows as typed.
function UserText({ text }: { text: string }) {
  return <div className="tt-message-text">{text}</div>
}

// The agent's replies are Markdown.
function AgentText({ text }: { text: string }) {
  return <Markdown text={text} headingShift={2} lineBreaks />
}

// What the agent's turn ended on: the provider's error, or that it was stopped.
function EntryError({ error }: { error: string }) {
  return <p className="tt-error">{error}</p>
}
