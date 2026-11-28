import { type FormEvent, useRef, useState } from 'react'
import { api, type Conversation, errorText } from '../api'
import { useT } from '../i18n'
import { Badge, type BadgeTone } from '../ui/Badge'
import { Button, ButtonLink } from '../ui/Button'
import { Heading } from '../ui/Heading'
import { IconButton } from '../ui/IconButton'
import { Input } from '../ui/Input'
import { MenuItem, MenuLink, MoreMenu } from '../ui/Menu'
import { Notice } from '../ui/Notice'
import { Skeleton } from '../ui/Skeleton'
import { ShareDialog } from './ShareDialog'
import { conversationTitle } from './title'
import type { StreamStatus } from './useConversationStream'

const STATUS_LABEL = {
  connecting: 'conversations.status.connecting',
  live: 'conversations.status.live',
  reconnecting: 'conversations.status.reconnecting',
  gone: 'conversations.status.gone',
} as const satisfies Record<StreamStatus, string>

const STATUS_TONE = {
  connecting: 'neutral',
  live: 'success',
  reconnecting: 'warning',
  gone: 'danger',
} as const satisfies Record<StreamStatus, BadgeTone>

export function ConversationHead({
  workspace,
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
  workspace: number
  backHref: string
  conversation: Conversation | undefined
  failed: boolean
  url: string
  onChanged: () => void
  buildHref?: string
  browserEnabled: boolean
  browserOpen: boolean
  onToggleBrowser: () => void
  status: StreamStatus
}) {
  const t = useT()
  const archived = Boolean(conversation?.archived_at)
  const reader = Boolean(conversation) && conversation?.role !== 'owner'
  const [sharing, setSharing] = useState(false)
  const [editing, setEditing] = useState(false)
  const [title, setTitle] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
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
    requestAnimationFrame(() => renameButton.current?.focus())
  }
  const save = (e: FormEvent) => {
    e.preventDefault()
    patch({ title })
  }

  const showBuild = Boolean(buildHref) && !archived && !reader
  const showBrowser = browserEnabled && !reader
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
        {conversation && editing && !reader ? (
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
        {conversation && !reader && (
          <span className="ui-chat-actions">
            <Button variant="ghost" size="sm" onClick={() => setSharing(true)}>
              {t('conversations.share.button')}
            </Button>
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
        {(showBuild || showBrowser) && (
          <MoreMenu>
            {showBuild && buildHref && (
              <MenuLink icon="apps" href={buildHref}>
                {t('apps.builder.buildHere')}
              </MenuLink>
            )}
            {showBrowser && (
              <MenuItem icon="browser" onSelect={onToggleBrowser}>
                {browserOpen ? t('conversations.hideBrowser') : t('conversations.showBrowser')}
              </MenuItem>
            )}
          </MoreMenu>
        )}
      </div>
      {error && <Notice tone="danger">{error}</Notice>}
      {conversation && !reader && sharing && (
        <ShareDialog
          workspace={workspace}
          url={url}
          owner={conversation.owner.id}
          open={sharing}
          onClose={() => setSharing(false)}
        />
      )}
    </div>
  )
}
