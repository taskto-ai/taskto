// One entry of the transcript: the person's message, in a quiet bubble at the end side, or the
// agent's reply, marked with its dot, with the tools the agent used in it and the error it ended on.

import { useT } from '../i18n'
import { Markdown } from '../markdown'
import { AgentDot } from '../ui/AgentDot'
import { Notice } from '../ui/Notice'
import { VisuallyHidden } from '../ui/VisuallyHidden'
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
  if (message.role === 'user') {
    return (
      <article className="ui-chat-msg" data-role="user">
        <VisuallyHidden>{t('conversations.you')}</VisuallyHidden>
        {/* What the person typed shows as typed, never as Markdown. */}
        {message.text && <div className="ui-chat-msg-text">{message.text}</div>}
        {message.error && <Notice tone="danger">{message.error}</Notice>}
      </article>
    )
  }
  return (
    <article className="ui-chat-msg" data-role="assistant" aria-busy={message.pending || undefined}>
      <AgentDot className="ui-chat-msg-dot" />
      <div className="ui-chat-msg-body">
        <VisuallyHidden>{t('conversations.agent')}</VisuallyHidden>
        {/* The agent's replies are Markdown. */}
        {message.text && (
          <div className="ui-chat-markdown">
            <Markdown text={message.text} headingShift={2} lineBreaks />
          </div>
        )}
        {message.tools.map((tool) => (
          <ToolLine key={tool.id} tool={tool} workspace={workspace} launched={apps.has(tool)} />
        ))}
        {message.pending && !message.text && <span className="ui-chat-pending">{'…'}</span>}
        {/* What the turn ended on: the provider's error, or that it was stopped. */}
        {message.error && <Notice tone="danger">{message.error}</Notice>}
      </div>
    </article>
  )
}
