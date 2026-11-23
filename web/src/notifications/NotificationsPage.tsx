// The notification inbox: what happened that the person may want to see, newest first, in the
// workspace shown. It works in every browser, whether or not it can push, and is what is true: a
// push is only a prompt to come here. Each item is a link to where it leads, which checks access
// again when it opens; an item whose subject the person can no longer read says only that
// something changed.

import { useCallback, useEffect, useState } from 'react'
import { api, errorText } from '../api'
import { useFormat, useT } from '../i18n'
import { Link, navigate, routePath } from '../router'
import { INBOX_CHANGED, inboxPath, type NotificationItem, type NotificationList } from './api'

const PAGE = 30

export function NotificationsPage({ workspace }: { workspace: number }) {
  const t = useT()
  const f = useFormat()
  const base = inboxPath(workspace)
  const [items, setItems] = useState<NotificationItem[] | null>(null)
  const [more, setMore] = useState(false)
  const [unread, setUnread] = useState(0)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

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
    <div className="tt-section">
      <div className="tt-pane-head">
        <h1>{t('notifications.title')}</h1>
        <button type="button" className="tt-button" disabled={busy || unread === 0} onClick={markAll}>
          {t('notifications.markAllRead')}
        </button>
      </div>
      <p className="tt-muted tt-small">
        {t('notifications.intro')}{' '}
        <Link href={routePath({ workspace, section: 'settings', item: 'notifications' })}>
          {t('notifications.pushSettings')}
        </Link>
      </p>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      {items === null && !error && <p className="tt-muted">{t('app.loading')}</p>}
      {items?.length === 0 && <p className="tt-muted">{t('notifications.none')}</p>}
      {items && items.length > 0 && (
        <ul className="tt-rows tt-inbox" aria-label={t('notifications.title')}>
          {items.map((item) => (
            <li key={item.id} className="tt-row" data-unread={item.read ? undefined : ''}>
              <div className="tt-row-main">
                <Link
                  href={routePath({ workspace: item.workspace_id, ...item.target })}
                  onClick={() => markRead(item)}
                  className="tt-inbox-link"
                >
                  {!item.read && <span className="tt-sr-only">{t('notifications.unread')} </span>}
                  {t(`notifications.kind.${item.kind}`, item.params)}
                </Link>
                <span className="tt-muted tt-small">{f.dateTime(item.created_at)}</span>
              </div>
            </li>
          ))}
        </ul>
      )}
      {more && items && (
        <button type="button" className="tt-button" onClick={() => void load(items.at(-1)?.id)}>
          {t('notifications.older')}
        </button>
      )}
    </div>
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
        <Link className="tt-button" href={routePath({ workspace: fallbackWorkspace, section: 'notifications' })}>
          {t('notifications.openInbox')}
        </Link>
      )}
    </div>
  )
}
