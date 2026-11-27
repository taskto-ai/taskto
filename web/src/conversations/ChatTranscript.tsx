import { type ReactNode, type RefObject, useEffect, useLayoutEffect, useRef } from 'react'
import { useT } from '../i18n'
import { AgentDot } from '../ui/AgentDot'
import { Button } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { Notice } from '../ui/Notice'
import { turnChanges } from './changes'
import { Message } from './Message'
import type { ChatMessage, ToolUse } from './transcript'

const FOLLOW_PX = 48

export function ChatTranscript({
  workspace,
  messages,
  started,
  running,
  truncated,
  apps,
  onLoadEarlier,
  loadingEarlier,
  earlierError,
  footer,
}: {
  workspace: number
  messages: ChatMessage[]
  started: boolean
  running: boolean
  truncated: boolean
  apps: Set<ToolUse>
  onLoadEarlier: () => void
  loadingEarlier: boolean
  earlierError: string | null
  footer?: ReactNode
}) {
  const t = useT()

  const scroller = useRef<HTMLDivElement>(null)
  const atBottom = useRef(true)
  const last = messages.at(-1)
  const changes = turnChanges(messages)

  const before = useRef<{ first: string | undefined; fromBottom: number } | null>(null)
  const firstKey = messages[0]?.key
  const loadEarlier = () => {
    const el = scroller.current
    if (el) before.current = { first: firstKey, fromBottom: el.scrollHeight - el.scrollTop }
    onLoadEarlier()
  }
  useLayoutEffect(() => {
    const kept = before.current
    if (!kept || kept.first === firstKey) return
    before.current = null
    const el = scroller.current
    if (el) el.scrollTop = el.scrollHeight - kept.fromBottom
  }, [firstKey])

  return (
    <div
      className="ui-chat-transcript"
      ref={scroller}
      onScroll={(e) => {
        const el = e.currentTarget
        atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < FOLLOW_PX
      }}
    >
      <div className="ui-chat-column">
        {truncated && (
          <div className="ui-chat-earlier">
            <Button size="sm" onClick={loadEarlier} disabled={loadingEarlier}>
              {loadingEarlier ? t('app.loading') : t('conversations.loadEarlier')}
            </Button>
            {earlierError && <Notice tone="danger">{earlierError}</Notice>}
          </div>
        )}
        {started && messages.length === 0 && <EmptyState title={t('conversations.start')} />}
        {messages.map((m) => (
          <Message key={m.key} message={m} workspace={workspace} apps={apps} changes={changes.get(m.key)} />
        ))}
        {footer}
        <FollowNewest
          key={JSON.stringify([
            last?.key ?? null,
            last?.text ?? null,
            Boolean(footer),
            (last && changes.get(last.key)?.length) ?? 0,
          ])}
          scroller={scroller}
          atBottom={atBottom}
        />
        {running && !last?.pending && (
          <p className="ui-chat-working">
            <AgentDot />
            {t('conversations.agentWorkingLive')}
          </p>
        )}
      </div>
    </div>
  )
}

function FollowNewest({
  scroller,
  atBottom,
}: {
  scroller: RefObject<HTMLDivElement | null>
  atBottom: RefObject<boolean>
}) {
  useEffect(() => {
    const el = scroller.current
    if (el && atBottom.current) el.scrollTop = el.scrollHeight
  }, [scroller, atBottom])
  return null
}
