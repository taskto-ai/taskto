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
const RECENT_MS = 24 * 60 * 60 * 1000
const REFRESH_MS = 30 * 1000

function useNow(interval: number): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), interval)
    return () => clearInterval(timer)
  }, [interval])
  return now
}

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
      <time className="ui-inbox-when" dateTime={at} tabIndex={0}>
        {f.relative(at, now)}
      </time>
    </Tooltip>
  )
}

function shownParams(
  params: NotificationItem['params'],
  calendarDate: (ymd: string) => string,
  untitled: string,
): NotificationItem['params'] {
  if (!params) return params
  const shown = { ...params }
  if (shown.due) shown.due = calendarDate(shown.due)
  if (shown.title === '') shown.title = untitled
  return shown
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
                    {t(
                      `notifications.kind.${item.kind}`,
                      shownParams(item.params, f.calendarDate, t('conversations.untitledShort')),
                    )}
                  </span>
                }
                detail={
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

export function OpenNotification({ id, fallbackWorkspace }: { id: number; fallbackWorkspace: number | null }) {
  const t = useT()
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
