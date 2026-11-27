import { useT } from '../i18n'
import { Markdown } from '../markdown'
import { AgentDot } from '../ui/AgentDot'
import { Notice } from '../ui/Notice'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import type { Changed } from './changes'
import { ToolLine } from './ToolLine'
import { TurnChanges } from './TurnChanges'
import type { ChatMessage, ToolUse } from './transcript'

export function Message({
  message,
  workspace,
  apps,
  changes,
}: {
  message: ChatMessage
  workspace: number
  apps: Set<ToolUse>
  changes?: Changed[]
}) {
  const t = useT()
  if (message.role === 'user') {
    return (
      <article className="ui-chat-msg" data-role="user">
        <VisuallyHidden>{t('conversations.you')}</VisuallyHidden>
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
        {message.text && (
          <div className="ui-chat-markdown">
            <Markdown text={message.text} headingShift={2} lineBreaks />
          </div>
        )}
        {message.tools.map((tool) => (
          <ToolLine key={tool.id} tool={tool} workspace={workspace} launched={apps.has(tool)} />
        ))}
        {changes && <TurnChanges changes={changes} workspace={workspace} />}
        {message.pending && !message.text && <span className="ui-chat-pending">{'…'}</span>}
        {message.error && <Notice tone="danger">{message.error}</Notice>}
      </div>
    </article>
  )
}
