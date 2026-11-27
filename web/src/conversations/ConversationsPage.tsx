import { useState } from 'react'
import { api, type Conversation, errorText, ws } from '../api'
import { builderPath } from '../apps/route'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { useMounted } from '../mounted'
import { navigate, routePath } from '../router'
import { steps } from '../session'
import { Button, ButtonLink } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { Heading } from '../ui/Heading'
import { IconButton } from '../ui/IconButton'
import { Notice } from '../ui/Notice'
import { Row, Rows } from '../ui/Rows'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { ConversationView } from './ConversationView'
import './Conversations.css'
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
  const mounted = useMounted()
  const [error, setError] = useState<string | null>(null)
  const openId = open && /^\d+$/.test(open) ? Number(open) : null
  const link = (id?: number) => routePath({ workspace, section: 'conversations', item: id ? String(id) : undefined })
  const buildHref = builderPath(workspace)

  const create = async () => {
    setCreating(true)
    setError(null)
    const s = steps(mounted)
    try {
      const c = await api<Conversation>(`${ws(workspace)}/conversations`, { method: 'POST', body: {} })
      if (!s.ok()) return
      list.reload()
      navigate(link(c.id))
    } catch (e) {
      if (s.ok()) setError(errorText(e))
    } finally {
      if (mounted.current) setCreating(false)
    }
  }

  return (
    <div className="ui-chat-layout" data-open={openId ? 'conversation' : 'list'}>
      <section className="ui-chat-list" aria-label={t('shell.sections.conversations')}>
        <div className="ui-chat-list-head">
          <Heading level={1} size="title" className="ui-chat-list-title">
            {archived ? t('conversations.archived') : t('shell.sections.conversations')}
          </Heading>
          <IconButton
            icon="archive"
            label={t('conversations.showArchivedTitle')}
            aria-pressed={archived}
            onClick={() => setArchived((a) => !a)}
          />
          <Button variant="primary" size="sm" icon="plus" onClick={create} loading={creating}>
            {t('conversations.new')}
          </Button>
        </div>
        {error && <Notice tone="danger">{error}</Notice>}
        {list.error && <Notice tone="danger">{list.error}</Notice>}
        {list.data === undefined && !list.error && (
          <div className="ui-chat-list-loading" aria-busy="true">
            <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
            <Skeleton width="70%" />
            <Skeleton width="40%" />
            <Skeleton width="80%" />
            <Skeleton width="35%" />
          </div>
        )}
        {list.data?.length === 0 &&
          (archived ? (
            <EmptyState title={t('conversations.nothingArchived')} />
          ) : (
            <EmptyState
              title={t('conversations.none')}
              actions={
                <ButtonLink href={buildHref} icon="apps" size="sm">
                  {t('apps.builder.build')}
                </ButtonLink>
              }
            >
              {t('conversations.noneHelp')}
            </EmptyState>
          ))}
        {list.data && list.data.length > 0 && (
          <div className="ui-chat-rows">
            <Rows label={archived ? t('conversations.archived') : t('shell.sections.conversations')}>
              {list.data.map((c) => (
                <Row
                  key={c.id}
                  href={link(c.id)}
                  current={c.id === openId}
                  title={conversationTitle(c)}
                  detail={
                    <>
                      {c.origin && <span className="ui-chat-origin">{t(`conversations.origin.${c.origin.kind}`)}</span>}
                      <time dateTime={c.updated_at} title={f.dateTime(c.updated_at)}>
                        {f.relative(c.updated_at)}
                      </time>
                    </>
                  }
                />
              ))}
            </Rows>
          </div>
        )}
        {list.data && list.data.length > 0 && !archived && (
          <div className="ui-chat-list-foot">
            <ButtonLink href={buildHref} icon="apps" variant="ghost" size="sm">
              {t('apps.builder.build')}
            </ButtonLink>
          </div>
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
        <section className="ui-chat ui-chat-none">
          <EmptyState title={t('conversations.pick')} />
        </section>
      )}
    </div>
  )
}
