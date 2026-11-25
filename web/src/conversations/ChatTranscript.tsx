// The transcript: earlier messages on demand, the messages, and while the agent works and hasn't
// begun its reply, a line saying so. It scrolls on its own, never the page.

import { useEffect, useLayoutEffect, useRef } from 'react'
import { useT } from '../i18n'
import { AgentDot } from '../ui/AgentDot'
import { Button } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { Notice } from '../ui/Notice'
import { Message } from './Message'
import type { ChatMessage, ToolUse } from './transcript'

// How near the bottom the reader must be for the transcript to follow new text.
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
}: {
  workspace: number
  messages: ChatMessage[]
  // The stream has shown the conversation: with no messages, it has none yet.
  started: boolean
  running: boolean
  // There are messages before the first one shown.
  truncated: boolean
  // The tool uses whose app shows launched.
  apps: Set<ToolUse>
  onLoadEarlier: () => void
  loadingEarlier: boolean
  earlierError: string | null
}) {
  const t = useT()

  // Follows the newest text as it grows, but only while the reader is at the bottom: someone
  // who scrolled up to re-read stays where they are.
  const scroller = useRef<HTMLDivElement>(null)
  const atBottom = useRef(true)
  const last = messages.at(-1)
  // biome-ignore lint/correctness/useExhaustiveDependencies: follows the newest text as it grows
  useEffect(() => {
    const el = scroller.current
    if (el && atBottom.current) el.scrollTop = el.scrollHeight
  }, [last?.key, last?.text])

  // Earlier messages go in above what the reader is looking at, which stays where it is.
  const fromBottom = useRef<number | null>(null)
  const loadEarlier = () => {
    const el = scroller.current
    if (el) fromBottom.current = el.scrollHeight - el.scrollTop
    onLoadEarlier()
  }
  // biome-ignore lint/correctness/useExhaustiveDependencies: runs when earlier messages arrive
  useLayoutEffect(() => {
    const el = scroller.current
    if (el && fromBottom.current !== null) el.scrollTop = el.scrollHeight - fromBottom.current
    fromBottom.current = null
  }, [messages[0]?.key])

  return (
    <div
      className="ui-chat-transcript"
      ref={scroller}
      onScroll={(e) => {
        const el = e.currentTarget
        atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < FOLLOW_PX
      }}
    >
      {/* At a reading width, in the middle of the pane. */}
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
          <Message key={m.key} message={m} workspace={workspace} apps={apps} />
        ))}
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
