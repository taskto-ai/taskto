// The conversation's head: back to the list, its title (renamed in place), archiving, the link
// to build an app in it, the agent browser's switch, and the events stream's status.

import { type FormEvent, useState } from 'react'
import { api, type Conversation, errorText } from '../api'
import { useT } from '../i18n'
import { Link } from '../router'
import { Icon } from '../shell/Icon'
import { conversationTitle } from './title'
import type { StreamStatus } from './useConversationStream'

const STATUS_LABEL = {
  connecting: 'conversations.status.connecting',
  live: 'conversations.status.live',
  reconnecting: 'conversations.status.reconnecting',
  gone: 'conversations.status.gone',
} as const satisfies Record<StreamStatus, string>

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
  return (
    <div className="tt-pane-head">
      <Link href={backHref} className="tt-icon-button tt-back" aria-label={t('conversations.allConversations')}>
        <Icon name="back" />
      </Link>
      {conversation ? (
        <TitleBar conversation={conversation} url={url} onChanged={onChanged} />
      ) : (
        <h2 className="tt-chat-title">{failed ? t('conversations.conversation') : '…'}</h2>
      )}
      {buildHref && !archived && (
        <Link
          href={buildHref}
          className="tt-icon-button"
          aria-label={t('apps.builder.buildHere')}
          title={t('apps.builder.buildHere')}
        >
          <Icon name="apps" />
        </Link>
      )}
      {browserEnabled && (
        <button
          type="button"
          className="tt-icon-button"
          aria-label={browserOpen ? t('conversations.hideBrowser') : t('conversations.showBrowser')}
          aria-pressed={browserOpen}
          onClick={onToggleBrowser}
        >
          <Icon name="browser" />
        </button>
      )}
      <span className="tt-status" data-status={status}>
        {t(STATUS_LABEL[status])}
      </span>
    </div>
  )
}

function TitleBar({
  conversation,
  url,
  onChanged,
}: {
  conversation: Conversation
  url: string
  onChanged: () => void
}) {
  const t = useT()
  const [editing, setEditing] = useState(false)
  const [title, setTitle] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const archived = Boolean(conversation.archived_at)

  const patch = async (body: { title?: string; archived?: boolean }) => {
    setBusy(true)
    setError(null)
    try {
      await api(url, { method: 'PATCH', body })
      setEditing(false)
      onChanged()
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }
  const save = (e: FormEvent) => {
    e.preventDefault()
    patch({ title })
  }

  if (editing) {
    return (
      <form className="tt-chat-title tt-rename" onSubmit={save}>
        <input
          aria-label={t('conversations.titleField')}
          value={title}
          maxLength={200}
          onChange={(e) => setTitle(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Escape') setEditing(false)
          }}
          // biome-ignore lint/a11y/noAutofocus: the field the reader just asked for
          autoFocus
        />
        <button type="submit" className="tt-button tt-primary" disabled={busy}>
          {t('conversations.save')}
        </button>
        <button type="button" className="tt-button" onClick={() => setEditing(false)}>
          {t('common.cancel')}
        </button>
        {error && (
          <p role="alert" className="tt-error tt-small">
            {error}
          </p>
        )}
      </form>
    )
  }
  return (
    <>
      <h2 className="tt-chat-title">{conversationTitle(conversation)}</h2>
      {error && (
        <p role="alert" className="tt-error tt-small">
          {error}
        </p>
      )}
      <button
        type="button"
        className="tt-icon-button"
        aria-label={t('conversations.rename')}
        title={t('conversations.rename')}
        onClick={() => {
          setTitle(conversation.title)
          setError(null)
          setEditing(true)
        }}
      >
        <Icon name="edit" />
      </button>
      <button
        type="button"
        className="tt-icon-button"
        aria-label={archived ? t('conversations.unarchive') : t('conversations.archive')}
        title={archived ? t('conversations.unarchive') : t('conversations.archive')}
        aria-pressed={archived}
        disabled={busy}
        onClick={() => patch({ archived: !archived })}
      >
        <Icon name="archive" />
      </button>
    </>
  )
}
