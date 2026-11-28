import { useEffect, useRef, useState } from 'react'
import { AppView } from '../apps/AppView'
import { appPath } from '../apps/route'
import type { AppTransport } from '../apps/transport'
import { useFormat, useT } from '../i18n'
import { BRAND } from '../shell/nav'
import { Button, ButtonLink } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { Icon } from '../ui/Icon'
import { Notice } from '../ui/Notice'
import { Page } from '../ui/Page'
import { PageHeader } from '../ui/PageHeader'
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { useOfflineData, useRuntime } from './hooks'
import { OutboxList, SyncNotice } from './OutboxList'
import type { Runtime } from './runtime'
import { type Download, kindOf, type OutboxItem, type Scope, scopeKey } from './store'
import { type OfflineTransport, offlineTransport } from './transport'
import { WriterNotice } from './WriterNotice'
import './OfflineShell.css'

export function OfflineShell({
  user,
  onReconnect,
  requested,
}: {
  user: string
  onReconnect: () => void
  requested?: Scope
}) {
  const t = useT()
  const { runtime, problem } = useRuntime()
  const data = useOfflineData(runtime)
  const [chosen, setChosen] = useState<string | null | undefined>(undefined)
  const requestedKey = requested ? scopeKey(requested) : null
  const [seenKey, setSeenKey] = useState(requestedKey)
  if (seenKey !== requestedKey) {
    setSeenKey(requestedKey)
    setChosen(undefined)
  }
  const open = chosen === undefined ? requestedKey : chosen
  const download = data.downloads.find((d) => d.key === open)
  const standalone = chosen === undefined && download !== undefined
  const missing = chosen === undefined && requestedKey !== null && data.loaded && !download
  const name = standalone ? download.name : undefined

  useEffect(() => {
    if (!name) return
    const before = document.title
    document.title = `${name} · ${BRAND}`
    return () => {
      document.title = before
    }
  }, [name])

  useEffect(() => {
    window.addEventListener('online', onReconnect)
    return () => window.removeEventListener('online', onReconnect)
  }, [onReconnect])

  const apps = Object.fromEntries(data.downloads.map((d) => [d.key, d.name]))
  return (
    <main className="ui-offline-shell" id="main">
      <Page width="list">
        <PageHeader
          title={standalone ? download.name : t('offline.shell.title')}
          help={
            <>
              {t('offline.shell.intro')} <span className="ui-offline-shell-user">{user}</span>
            </>
          }
          actions={
            <>
              {standalone && requested && (
                <ButtonLink
                  variant="ghost"
                  icon="apps"
                  href={appPath(requested.workspace, requested.space, requested.app)}
                >
                  {t('apps.openInTaskto')}
                </ButtonLink>
              )}
              <Button icon="offline" onClick={onReconnect}>
                {t('offline.shell.reconnect')}
              </Button>
            </>
          }
        />
        {missing && <Notice tone="neutral">{t('offline.shell.notDownloaded')}</Notice>}
        {problem === 'schema' && (
          <Notice
            tone="danger"
            action={
              <Button variant="primary" onClick={() => window.location.reload()}>
                {t('offline.reload')}
              </Button>
            }
          >
            {t('offline.schemaNewer')}
          </Notice>
        )}
        {problem === 'unavailable' && <Notice tone="neutral">{t('offline.unavailable')}</Notice>}
        {runtime && <WriterNotice runtime={runtime} />}
        {runtime && !download && (
          <>
            {data.loaded && data.downloads.length === 0 && (
              <EmptyState title={t('offline.title')}>{t('offline.shell.none')}</EmptyState>
            )}
            {data.downloads.length > 0 && (
              <Panel>
                <Rows label={t('offline.title')}>
                  {data.downloads.map((d) => (
                    <Row
                      key={d.key}
                      leading={<Icon name="apps" />}
                      title={d.name}
                      actions={
                        <Button
                          size="sm"
                          aria-label={t('offline.shell.openNamed', { name: d.name })}
                          onClick={() => setChosen(d.key)}
                        >
                          {t('offline.shell.open')}
                        </Button>
                      }
                    />
                  ))}
                </Rows>
              </Panel>
            )}
          </>
        )}
        {runtime && download && (
          <>
            <Button variant="ghost" icon="back" className="ui-offline-shell-back" onClick={() => setChosen(null)}>
              {t('offline.shell.back')}
            </Button>
            <OfflineApp
              key={download.key}
              runtime={runtime}
              download={download}
              items={data.items.filter((i) => i.scopeKey === download.key)}
            />
          </>
        )}
        {runtime && (
          <OutboxList
            runtime={runtime}
            items={data.items}
            status={data.status}
            apps={apps}
            downloads={data.downloads}
          />
        )}
        {runtime && data.items.length === 0 && <SyncNotice status={data.status} waiting={0} />}
      </Page>
    </main>
  )
}

export function OfflineApp({
  runtime,
  download,
  items,
}: {
  runtime: Runtime
  download: Download
  items: OutboxItem[]
}) {
  const t = useT()
  const f = useFormat()
  const [saved, setSaved] = useState(false)
  const [reload, setReload] = useState(0)
  const made = useRef<{ key: string; transport: OfflineTransport } | null>(null)
  const key = `${download.key}@${download.capturedAt}`
  if (made.current?.key !== key) {
    made.current?.transport.photos.release()
    made.current = { key, transport: offlineTransport(runtime, download, { onSaved: () => setSaved(true) }) }
  }
  const transport: AppTransport = made.current.transport
  useEffect(() => () => made.current?.transport.photos.release(), [])
  const queued = items.length
  const changes = items.filter((i) => kindOf(i) !== 'insert' && i.state !== 'synced').length

  const signature = items
    .map(
      (i) =>
        `${i.id}:${kindOf(i) === 'insert' ? '' : i.state === 'synced' ? (i.ack?._revision ?? '') : i.record.revision}`,
    )
    .join(',')
  const seen = useRef<string[]>(signature ? signature.split(',') : [])
  useEffect(() => {
    const now = new Map(signature ? signature.split(',').map((e) => [e.slice(0, e.lastIndexOf(':')), e] as const) : [])
    if (seen.current.some((e) => now.get(e.slice(0, e.lastIndexOf(':'))) !== e)) setReload((n) => n + 1)
    seen.current = signature ? signature.split(',') : []
  }, [signature])

  return (
    <div className="ui-offline-app">
      <p className="ui-offline-app-note" role="note">
        {t('offline.localCopy', {
          when: f.dateTime(download.capturedAt),
          records: f.number(download.records),
          queued: f.number(queued),
        })}
        {download.truncated
          ? ` ${t('offline.localTruncated', { records: f.number(download.records), total: f.number(download.total) })}`
          : ''}
      </p>
      {changes > 0 && <p className="ui-offline-app-note">{t('offline.pendingChanges', { count: changes })}</p>}
      {saved && <Notice tone="success">{t('offline.saved')}</Notice>}
      <AppView
        key={`${scopeKey(download)}@${download.release}`}
        surfaceId={`app-${download.app}`}
        label={download.name}
        transport={transport}
        release={download.definition}
        reloadKey={reload}
        onAction={() => undefined}
      />
    </div>
  )
}
