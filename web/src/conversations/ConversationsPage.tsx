// The user's conversations in a workspace: the list beside the open one. On a phone it is
// one or the other, with a way back to the list. The list shows the active conversations, or
// the archived ones.

import { useState } from 'react'
import { api, type Conversation, errorText, ws } from '../api'
import { builderPath } from '../apps/route'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Link, navigate, routePath } from '../router'
import { Icon } from '../shell/Icon'
import { ConversationView } from './ConversationView'
import { conversationTitle } from './title'
import type { EventSourceFactory } from './useConversationStream'

export function ConversationsPage({
  workspace,
  open,
  streams,
}: {
  workspace: number
  open?: string
  streams?: EventSourceFactory
}) {
  const t = useT()
  const f = useFormat()
  const [archived, setArchived] = useState(false)
  const list = useLoad(
    () =>
      api<{ conversations: Conversation[] }>(`${ws(workspace)}/conversations${archived ? '?archived=true' : ''}`).then(
        (r) => r.conversations,
      ),
    [workspace, archived],
  )
  const [creating, setCreating] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const openId = open && /^\d+$/.test(open) ? Number(open) : null
  const link = (id?: number) => routePath({ workspace, section: 'conversations', item: id ? String(id) : undefined })

  const create = async () => {
    setCreating(true)
    setError(null)
    try {
      const c = await api<Conversation>(`${ws(workspace)}/conversations`, { method: 'POST', body: {} })
      list.reload()
      navigate(link(c.id))
    } catch (e) {
      setError(errorText(e))
    } finally {
      setCreating(false)
    }
  }

  return (
    <div className="tt-chat-layout" data-open={openId ? 'conversation' : 'list'}>
      <section className="tt-chat-list" aria-label={t('shell.sections.conversations')}>
        <div className="tt-pane-head">
          <h1>{archived ? t('conversations.archived') : t('shell.sections.conversations')}</h1>
          <button
            type="button"
            className="tt-icon-button"
            aria-pressed={archived}
            aria-label={t('conversations.showArchived')}
            title={archived ? t('conversations.showActiveTitle') : t('conversations.showArchivedTitle')}
            onClick={() => setArchived((a) => !a)}
          >
            <Icon name="archive" />
          </button>
          <button type="button" className="tt-button tt-primary" onClick={create} disabled={creating}>
            <Icon name="plus" />
            {t('conversations.new')}
          </button>
          <Link className="tt-button" href={builderPath(workspace)}>
            <Icon name="apps" />
            {t('apps.builder.build')}
          </Link>
        </div>
        {error && (
          <p role="alert" className="tt-error">
            {error}
          </p>
        )}
        {list.error && (
          <p role="alert" className="tt-error">
            {list.error}
          </p>
        )}
        {list.data === undefined && !list.error && <p className="tt-muted">{t('app.loading')}</p>}
        {list.data?.length === 0 && (
          <p className="tt-muted">{archived ? t('conversations.nothingArchived') : t('conversations.none')}</p>
        )}
        {list.data && list.data.length > 0 && (
          <ul className="tt-list">
            {list.data.map((c) => (
              <li key={c.id}>
                <Link href={link(c.id)} aria-current={c.id === openId ? 'page' : undefined}>
                  <span className="tt-list-title">{conversationTitle(c)}</span>
                  <span className="tt-muted tt-small">{f.dateTime(c.updated_at)}</span>
                </Link>
              </li>
            ))}
          </ul>
        )}
      </section>
      {openId ? (
        <ConversationView
          key={openId}
          workspace={workspace}
          id={openId}
          backHref={link()}
          onChanged={list.reload}
          streams={streams}
          buildHref={builderPath(workspace, { kind: 'conversation', conversation: openId })}
        />
      ) : (
        <section className="tt-chat tt-chat-empty">
          <p className="tt-muted">{t('conversations.pick')}</p>
        </section>
      )}
    </div>
  )
}
