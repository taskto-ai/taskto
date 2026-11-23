// What the app shows when the server can't be reached and someone signed in here before: the apps
// they downloaded, usable without it, and the records made on this device waiting to be sent.
//
// This is a different place from the signed-in app on purpose. It has no session to speak of (the
// browser's cookie can't be checked offline), so it shows only this user's own downloads, from the
// database of their id, and does nothing the server decides: records are queued, and the server
// is asked again, with the session the browser holds then, before any is accepted.

import { useEffect, useRef, useState } from 'react'
import { type AppTransport, AppView } from '../apps/AppView'
import { useFormat, useT } from '../i18n'
import { useOfflineData, useRuntime } from './hooks'
import { OutboxList, SyncNotice } from './OutboxList'
import type { Runtime } from './runtime'
import { type Download, kindOf, type OutboxItem, scopeKey } from './store'
import { type OfflineTransport, offlineTransport } from './transport'
import { WriterNotice } from './WriterNotice'

export function OfflineShell({ user, onReconnect }: { user: string; onReconnect: () => void }) {
  const t = useT()
  const { runtime, problem } = useRuntime()
  const data = useOfflineData(runtime)
  const [open, setOpen] = useState<string | null>(null)
  const download = data.downloads.find((d) => d.key === open)

  // Back online: the page asks the server who is signed in, and the signed-in app takes over.
  useEffect(() => {
    window.addEventListener('online', onReconnect)
    return () => window.removeEventListener('online', onReconnect)
  }, [onReconnect])

  const apps = Object.fromEntries(data.downloads.map((d) => [d.key, d.name]))
  return (
    <main className="tt-section tt-offline-shell" id="main">
      <div className="tt-pane-head">
        <h1>{t('offline.shell.title')}</h1>
        <span className="tt-muted">{user}</span>
        <button type="button" className="tt-button" onClick={onReconnect}>
          {t('offline.shell.reconnect')}
        </button>
      </div>
      <p className="tt-muted">{t('offline.shell.intro')}</p>
      {problem === 'schema' && (
        <div role="alert" className="tt-error">
          <p>{t('offline.schemaNewer')}</p>
          <button type="button" className="tt-button tt-primary" onClick={() => window.location.reload()}>
            {t('offline.reload')}
          </button>
        </div>
      )}
      {problem === 'unavailable' && <p className="tt-muted">{t('offline.unavailable')}</p>}
      {runtime && <WriterNotice runtime={runtime} />}
      {runtime && !download && (
        <>
          {data.loaded && data.downloads.length === 0 && <p className="tt-muted">{t('offline.shell.none')}</p>}
          <ul className="tt-list" aria-label={t('offline.title')}>
            {data.downloads.map((d) => (
              <li key={d.key}>
                <button
                  type="button"
                  aria-label={t('offline.shell.openNamed', { name: d.name })}
                  onClick={() => setOpen(d.key)}
                >
                  <span className="tt-list-title">{d.name}</span>
                </button>
              </li>
            ))}
          </ul>
        </>
      )}
      {runtime && download && (
        <>
          <button type="button" className="tt-button" onClick={() => setOpen(null)}>
            {t('offline.shell.back')}
          </button>
          <OfflineApp
            runtime={runtime}
            download={download}
            items={data.items.filter((i) => i.scopeKey === download.key)}
          />
        </>
      )}
      {runtime && (
        <OutboxList runtime={runtime} items={data.items} status={data.status} apps={apps} downloads={data.downloads} />
      )}
      {runtime && data.items.length === 0 && <SyncNotice status={data.status} waiting={0} />}
    </main>
  )
}

// One downloaded app, drawn from the device. Above it, what its numbers are.
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
  // A new transport only for a different copy: the photos staged in the form live in it.
  const made = useRef<{ key: string; transport: OfflineTransport } | null>(null)
  const key = `${download.key}@${download.capturedAt}`
  if (made.current?.key !== key) {
    made.current?.transport.photos.release()
    made.current = { key, transport: offlineTransport(runtime, download, { onSaved: () => setSaved(true) }) }
  }
  const transport: AppTransport = made.current.transport
  useEffect(() => () => made.current?.transport.photos.release(), [])
  const queued = items.length
  // Changes to downloaded records that the server doesn't have yet: shown on the screen as the
  // person left them, and said to be waiting.
  const changes = items.filter((i) => kindOf(i) !== 'insert' && i.state !== 'synced').length

  // A record that left the queue behind this screen's back (discarded, cleared), or a change to a
  // record whose revision moved on (settled by the person, acknowledged by the server), is drawn
  // again, so the next change is based on the revision now. A record added by this screen's own
  // button is already in the answer, and the form being typed in is not drawn over.
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
    <div className="tt-stage" data-stage="offline">
      <p className="tt-muted tt-small" role="note">
        {t('offline.localCopy', {
          when: f.dateTime(download.capturedAt),
          records: f.number(download.records),
          queued: f.number(queued),
        })}
        {download.truncated
          ? ` ${t('offline.localTruncated', { records: f.number(download.records), total: f.number(download.total) })}`
          : ''}
      </p>
      {changes > 0 && <p className="tt-muted tt-small">{t('offline.pendingChanges', { count: changes })}</p>}
      {saved && (
        <p role="status" className="tt-offline-saved">
          {t('offline.saved')}
        </p>
      )}
      <AppView
        // A screen is drawn from one release of the app, and a button on it is resolved in that one: a
        // download that brought another release is drawn again, a fresh screen with its own buttons,
        // never the old one's pressed under the new definition.
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
