// The notification inbox: what happened that the person may want to see, newest first, in the
// workspace shown, as rows: what happened, where and when, an unread one in bold with a mark before
// it (and "Unread:" said before it, so the mark is never the only sign). When is "5 minutes ago"
// for the last day, kept current while the page is open, with the date and time on hover and
// focus; the date and time after that. It works in every browser, whether or not it can push, and
// is what is true: a push is only a prompt to come here. Each item is a link to where it leads, which checks access
// again when it opens; an item whose subject the person can no longer read says only that
// something changed.

import { useCallback, useEffect, useState } from 'react'
import { ApiError, api, errorText } from '../api'
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
import { Tooltip } from '../ui/Tooltip'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { INBOX_CHANGED, inboxPath, type NotificationItem, type NotificationList } from './api'
import './Inbox.css'

const PAGE = 30
// How long "how long ago" stays the way a time is said, and how often it is said again.
const RECENT_MS = 24 * 60 * 60 * 1000
const REFRESH_MS = 30 * 1000

// The time now, again every `interval` while the page is open.
function useNow(interval: number): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), interval)
    return () => clearInterval(timer)
  }, [interval])
  return now
}

// When a notification arrived: relative for the last day, with the date and time a hover or the
// keyboard shows (and a screen reader reads as its description); the date and time after that.
function When({ at, now }: { at: string; now: number }) {
  const f = useFormat()
  const full = f.dateTime(at)
  if (now - new Date(at).getTime() >= RECENT_MS) {
    return (
      <time className="ui-inbox-when" dateTime={at}>
        {full}
      </time>
    )
  }
  return (
    <Tooltip content={full}>
      {/* biome-ignore lint/a11y/noNoninteractiveTabindex: the keyboard reaches it to show the date and time, as a hover does */}
      <time className="ui-inbox-when" dateTime={at} tabIndex={0}>
        {f.relative(at, now)}
      </time>
    </Tooltip>
  )
}

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
  const now = useNow(REFRESH_MS)

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
                detail={
                  // A "changed" item says nothing of its subject, so not where it was either.
                  item.kind === 'changed' ? (
                    <When at={item.created_at} now={now} />
                  ) : (
                    <>
                      {t('notifications.in', { where: t(`shell.sections.${item.target.section}` as Key) })}{' '}
                      <When at={item.created_at} now={now} />
                    </>
                  )
                }
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
  // Gone: the server answered that there is no such notification for this person (404, or 403).
  // Failed: anything else (no answer, the server's own failure), which says nothing about it.
  const [outcome, setOutcome] = useState<'waiting' | 'gone' | 'failed'>('waiting')
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
      (e) => live && setOutcome(e instanceof ApiError && (e.status === 404 || e.status === 403) ? 'gone' : 'failed'),
    )
    return () => {
      live = false
    }
  }, [id])
  const inbox = fallbackWorkspace !== null && (
    <ButtonLink href={routePath({ workspace: fallbackWorkspace, section: 'notifications' })}>
      {t('notifications.openInbox')}
    </ButtonLink>
  )
  return (
    <Page width="list">
      <PageHeader title={t('notifications.linkTitle')} />
      {outcome === 'waiting' && <InboxSkeleton />}
      {outcome === 'gone' && (
        <Panel>
          <EmptyState
            icon={<Icon name="bell" />}
            title={<span role="alert">{t('notifications.unavailable')}</span>}
            actions={inbox}
          />
        </Panel>
      )}
      {outcome === 'failed' && (
        <Notice
          tone="danger"
          action={
            <Button size="sm" onClick={() => window.location.reload()}>
              {t('app.tryAgain')}
            </Button>
          }
        >
          {t('notifications.linkFailed')}
        </Notice>
      )}
    </Page>
  )
}
