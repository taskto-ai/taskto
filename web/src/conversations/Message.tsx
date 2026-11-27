import { useT } from '../i18n'
import { Markdown } from '../markdown'
import { AgentDot } from '../ui/AgentDot'
import { Notice } from '../ui/Notice'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { leadingAgent } from './agentMention'
import { ToolLine } from './ToolLine'
import type { ChatMessage, ToolUse } from './transcript'

export function Message({ message, workspace, apps }: { message: ChatMessage; workspace: number; apps: Set<ToolUse> }) {
  const t = useT()
  if (message.role === 'user') {
    const agent = leadingAgent(message.text)
    return (
      <article className="ui-chat-msg" data-role="user">
        <VisuallyHidden>{t('conversations.you')}</VisuallyHidden>
        {message.text && (
          <div className="ui-chat-msg-text">
            {agent ? (
              <>
                <span className="ui-chat-agent-chip" title={t('conversations.agentChip', { agent: agent.label })}>
                  {`@${agent.label}`}
                </span>
                {agent.rest ? ` ${agent.rest}` : ''}
              </>
            ) : (
              message.text
            )}
          </div>
        )}
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
        {message.pending && !message.text && <span className="ui-chat-pending">{'…'}</span>}
        {message.error && <Notice tone="danger">{message.error}</Notice>}
      </div>
    </article>
  )
}
