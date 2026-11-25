// The notification inbox: what happened that the person may want to see, newest first, in the
// workspace shown, as rows: what happened, where and when, an unread one in bold with a mark before
// it (and "Unread:" said before it, so the mark is never the only sign). It works in every browser, whether or not it can push, and is what is true: a
// push is only a prompt to come here. Each item is a link to where it leads, which checks access
// again when it opens; an item whose subject the person can no longer read says only that
// something changed.

import { useCallback, useEffect, useState } from 'react'
import { api, errorText } from '../api'
import { type Key, useFormat, useT } from '../i18n'
import { Link, navigate, routePath } from '../router'
import { Button, ButtonLink } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { Icon } from '../ui/Icon'
import { Notice } from '../ui/Notice'
import { Page } from '../ui/Page'
import { PageHeader } from '../ui/PageHeader'
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { INBOX_CHANGED, inboxPath, type NotificationItem, type NotificationList } from './api'
import './Inbox.css'

const PAGE = 30

// A due date is a calendar date, which the language's own format says; the rest is shown as it is.
function shownParams(params: NotificationItem['params'], calendarDate: (ymd: string) => string) {
  if (!params?.due) return params
  return { ...params, due: calendarDate(params.due) }
}

export function NotificationsPage({ workspace }: { workspace: number }) {
  const t = useT()
  const f = useFormat()
  const base = inboxPath(workspace)
  const [items, setItems] = useState<NotificationItem[] | null>(null)
  const [more, setMore] = useState(false)
  const [unread, setUnread] = useState(0)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [older, setOlder] = useState(false)

  const load = useCallback(
    async (before?: number) => {
      setError(null)
      try {
        const page = await api<NotificationList>(`${base}?limit=${PAGE}${before ? `&before=${before}` : ''}`)
        setItems((old) => (before && old ? [...old, ...page.notifications] : page.notifications))
        setMore(page.more)
        setUnread(page.unread)
      } catch (e) {
        setError(errorText(e))
      }
    },
    [base],
  )
  useEffect(() => {
    setItems(null)
    void load()
  }, [load])
  const loadOlder = async (before?: number) => {
    setOlder(true)
    await load(before)
    setOlder(false)
  }

  const changed = () => window.dispatchEvent(new Event(INBOX_CHANGED))
  const markRead = (item: NotificationItem) => {
    if (item.read) return
    setItems((old) => old?.map((i) => (i.id === item.id ? { ...i, read: true } : i)) ?? old)
    setUnread((n) => Math.max(0, n - 1))
    api(`${base}/${item.id}/read`, { method: 'POST', body: {} }).then(changed, () => void load())
  }
  const markAll = async () => {
    setBusy(true)
    try {
      await api(`${base}/read`, { method: 'POST', body: {} })
      setItems((old) => old?.map((i) => ({ ...i, read: true })) ?? old)
      setUnread(0)
      changed()
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Page width="list">
      <PageHeader
        title={t('notifications.title')}
        help={
          <>
            {t('notifications.intro')}{' '}
            <Link href={routePath({ workspace, section: 'settings', item: 'notifications' })}>
              {t('notifications.pushSettings')}
            </Link>
          </>
        }
        actions={
          <Button icon="check" loading={busy} disabled={busy || unread === 0} onClick={markAll}>
            {t('notifications.markAllRead')}
          </Button>
        }
      />
      {error && <Notice tone="danger">{error}</Notice>}
      {items === null && !error && <InboxSkeleton />}
      {items?.length === 0 && (
        <EmptyState icon={<Icon name="bell" size={24} />} title={t('notifications.none')}>
          <p>{t('notifications.noneHelp')}</p>
        </EmptyState>
      )}
      {items && items.length > 0 && (
        <Panel>
          <Rows label={t('notifications.title')}>
            {items.map((item) => (
              <Row
                key={item.id}
                href={routePath({ workspace: item.workspace_id, ...item.target })}
                onOpen={() => markRead(item)}
                leading={<span className="ui-inbox-mark" data-unread={item.read ? undefined : true} />}
                title={
                  <span className="ui-inbox-title" data-unread={item.read ? undefined : true}>
                    {!item.read && <VisuallyHidden>{`${t('notifications.unread')} `}</VisuallyHidden>}
                    {t(`notifications.kind.${item.kind}`, shownParams(item.params, f.calendarDate))}
                  </span>
                }
                detail={t('notifications.where', {
                  where: t(`shell.sections.${item.target.section}` as Key),
                  when: f.relative(item.created_at),
                })}
              />
            ))}
          </Rows>
        </Panel>
      )}
      {more && items && (
        <div className="ui-inbox-more">
          <Button loading={older} onClick={() => void loadOlder(items.at(-1)?.id)}>
            {t('notifications.older')}
          </Button>
        </div>
      )}
    </Page>
  )
}

// While the inbox loads: the shape of a few rows, and a word for screen readers.
function InboxSkeleton() {
  const t = useT()
  return (
    <Panel aria-busy="true">
      <VisuallyHidden>
        <span role="status">{t('app.loading')}</span>
      </VisuallyHidden>
      <div className="ui-inbox-skeleton" aria-hidden="true">
        {['70%', '55%', '80%', '45%'].map((w) => (
          <div key={w} className="ui-inbox-skeleton-row">
            <Skeleton width={w} />
            <Skeleton width="25%" />
          </div>
        ))}
      </div>
    </Panel>
  )
}

/**
 * The address a push opens, /notification/{id}, which holds nothing but the notification's own
 * ID. It is read as the signed-in person (someone else's is as missing as one that never was),
 * marked read, and the person is taken to where it leads: that page checks their access again.
 */
export function OpenNotification({ id, fallbackWorkspace }: { id: number; fallbackWorkspace: number | null }) {
  const t = useT()
  const [gone, setGone] = useState(false)
  useEffect(() => {
    let live = true
    api<NotificationItem>(`/api/me/notifications/${id}`).then(
      (item) => {
        if (!live) return
        api(`${inboxPath(item.workspace_id)}/${item.id}/read`, { method: 'POST', body: {} }).then(
          () => window.dispatchEvent(new Event(INBOX_CHANGED)),
          () => {},
        )
        navigate(routePath({ workspace: item.workspace_id, ...item.target }), { replace: true })
      },
      () => live && setGone(true),
    )
    return () => {
      live = false
    }
  }, [id])
  if (!gone) return <p className="tt-center tt-muted">{t('app.loading')}</p>
  return (
    <div className="tt-center">
      <p role="alert">{t('notifications.unavailable')}</p>
      {fallbackWorkspace !== null && (
        <ButtonLink href={routePath({ workspace: fallbackWorkspace, section: 'notifications' })}>
          {t('notifications.openInbox')}
        </ButtonLink>
      )}
    </div>
  )
}
