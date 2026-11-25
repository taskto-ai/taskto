// The conversation's head: back to the list (on a phone), its title, renamed in place, the events
// stream's status, rename and archive, and in a menu the link to build an app in it and the agent
// browser's switch.

import { type FormEvent, useRef, useState } from 'react'
import { api, type Conversation, errorText } from '../api'
import { useT } from '../i18n'
import { navigate } from '../router'
import { Badge, type BadgeTone } from '../ui/Badge'
import { Button, ButtonLink } from '../ui/Button'
import { Heading } from '../ui/Heading'
import { IconButton } from '../ui/IconButton'
import { Input } from '../ui/Input'
import { MenuItem, MoreMenu } from '../ui/Menu'
import { Notice } from '../ui/Notice'
import { Skeleton } from '../ui/Skeleton'
import { conversationTitle } from './title'
import type { StreamStatus } from './useConversationStream'

const STATUS_LABEL = {
  connecting: 'conversations.status.connecting',
  live: 'conversations.status.live',
  reconnecting: 'conversations.status.reconnecting',
  gone: 'conversations.status.gone',
} as const satisfies Record<StreamStatus, string>

// The words say the state; the tone only repeats it.
const STATUS_TONE = {
  connecting: 'neutral',
  live: 'success',
  reconnecting: 'warning',
  gone: 'danger',
} as const satisfies Record<StreamStatus, BadgeTone>

export function ConversationHead({
  backHref,
  conversation,
  failed,
  url,
  onChanged,
  buildHref,
  browserEnabled,
  browserOpen,
  onToggleBrowser,
  status,
}: {
  backHref: string
  // The conversation once read; failed when it couldn't be.
  conversation: Conversation | undefined
  failed: boolean
  url: string
  // Renamed or archived.
  onChanged: () => void
  // Where to build an app in this conversation; no link without it.
  buildHref?: string
  // Whether the user has an agent browser, and whether its panel is open.
  browserEnabled: boolean
  browserOpen: boolean
  onToggleBrowser: () => void
  status: StreamStatus
}) {
  const t = useT()
  const archived = Boolean(conversation?.archived_at)
  const [editing, setEditing] = useState(false)
  const [title, setTitle] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // Where the focus goes back to when the rename ends.
  const renameButton = useRef<HTMLButtonElement>(null)

  const patch = async (body: { title?: string; archived?: boolean }) => {
    setBusy(true)
    setError(null)
    try {
      await api(url, { method: 'PATCH', body })
      if (body.title !== undefined) stopEditing()
      onChanged()
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }
  const stopEditing = () => {
    setEditing(false)
    // After the button is back on the page.
    requestAnimationFrame(() => renameButton.current?.focus())
  }
  const save = (e: FormEvent) => {
    e.preventDefault()
    patch({ title })
  }

  const showBuild = Boolean(buildHref) && !archived
  return (
    <div className="ui-chat-head-wrap">
      <div className="ui-chat-head">
        <ButtonLink
          href={backHref}
          variant="ghost"
          icon="back"
          className="ui-chat-back"
          aria-label={t('conversations.allConversations')}
        />
        {conversation && editing ? (
          <form className="ui-chat-rename" onSubmit={save}>
            <Input
              aria-label={t('conversations.titleField')}
              value={title}
              maxLength={200}
              size="sm"
              onChange={(e) => setTitle(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Escape') {
                  e.preventDefault()
                  stopEditing()
                }
              }}
              // The field the reader just asked for.
              autoFocus
            />
            <Button type="submit" size="sm" loading={busy}>
              {t('conversations.save')}
            </Button>
            <Button variant="ghost" size="sm" onClick={stopEditing}>
              {t('common.cancel')}
            </Button>
          </form>
        ) : (
          <Heading level={2} size="subsection" className="ui-chat-title">
            {conversation ? (
              conversationTitle(conversation)
            ) : failed ? (
              t('conversations.conversation')
            ) : (
              <Skeleton width="12rem" />
            )}
          </Heading>
        )}
        <Badge tone={STATUS_TONE[status]} dot className="ui-chat-status">
          {t(STATUS_LABEL[status])}
        </Badge>
        {conversation && (
          <span className="ui-chat-actions">
            {!editing && (
              <IconButton
                ref={renameButton}
                icon="edit"
                label={t('conversations.rename')}
                onClick={() => {
                  setTitle(conversation.title)
                  setError(null)
                  setEditing(true)
                }}
              />
            )}
            <IconButton
              icon="archive"
              label={archived ? t('conversations.unarchive') : t('conversations.archive')}
              aria-pressed={archived}
              disabled={busy}
              onClick={() => patch({ archived: !archived })}
            />
          </span>
        )}
        {(showBuild || browserEnabled) && (
          <MoreMenu>
            {showBuild && buildHref && (
              <MenuItem icon="apps" onSelect={() => navigate(buildHref)}>
                {t('apps.builder.buildHere')}
              </MenuItem>
            )}
            {browserEnabled && (
              <MenuItem icon="browser" onSelect={onToggleBrowser}>
                {browserOpen ? t('conversations.hideBrowser') : t('conversations.showBrowser')}
              </MenuItem>
            )}
          </MoreMenu>
        )}
      </div>
      {error && <Notice tone="danger">{error}</Notice>}
    </div>
  )
}
