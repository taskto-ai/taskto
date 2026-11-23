// The records of one launched app, with their history: which records there are, what
// happened to each (made, changed, deleted, restored, by whom and when), and the deleted ones,
// which can be restored. Shown to whoever may read the app's records; restoring takes the right
// to change them. The server decides what each person reads and restores: this only asks.

import { useState } from 'react'
import { type AppAccess, type AppRecord, api, errorText, type RecordAuthor, type RecordVersion } from '../api'
import { type Format, type T, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { PhotoStrip } from './PhotoInput'
import { photoIds } from './photos'

type Tab = 'live' | 'deleted'
type RecordList = { records: AppRecord[]; truncated: boolean }

export function AppRecords({
  base,
  access,
  writable,
  onRestored,
}: {
  // …/spaces/{space}/apps/{app}
  base: string
  access: AppAccess
  // Whether the space lets the caller write; a restore is a write.
  writable: boolean
  // Called after a record was restored, so the screen shows it again.
  onRestored: () => void
}) {
  const [open, setOpen] = useState(false)
  const t = useT()
  if (access.read_records === 'none') return null
  return (
    <div className="tt-records">
      <button
        type="button"
        className="tt-button"
        aria-expanded={open}
        aria-controls="tt-records-panel"
        onClick={() => setOpen((o) => !o)}
      >
        {open ? t('apps.records.close') : t('apps.records.open')}
      </button>
      {open && (
        <RecordsPanel base={base} canRestore={writable && access.modify_records !== 'none'} onRestored={onRestored} />
      )}
    </div>
  )
}

function RecordsPanel({ base, canRestore, onRestored }: { base: string; canRestore: boolean; onRestored: () => void }) {
  const [tab, setTab] = useState<Tab>('live')
  const [history, setHistory] = useState<number | null>(null)
  const [busy, setBusy] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const t = useT()
  const f = useFormat()
  const list = useLoad(() => api<RecordList>(`${base}/records${tab === 'deleted' ? '/deleted' : ''}`), [base, tab])

  const choose = (next: Tab) => {
    setTab(next)
    setHistory(null)
    setError(null)
    setNotice(null)
  }
  // Restoring names the revision of the deleted record the person saw: if it changed since,
  // the server refuses and the list is shown as it is now.
  const restore = async (rec: AppRecord) => {
    setBusy(rec.id)
    setNotice(null)
    try {
      await api(`${base}/records/${rec.id}/restore`, { method: 'POST', body: { expected_revision: rec.revision } })
      setError(null)
      setNotice(t('apps.records.restored', { id: String(rec.id) }))
      onRestored()
    } catch (e) {
      setError(errorText(e))
    }
    setBusy(null)
    list.reload()
  }

  return (
    <div id="tt-records-panel" className="tt-records-panel">
      <p className="tt-muted tt-small">{t('apps.records.intro')}</p>
      <nav className="tt-records-tabs" aria-label={t('apps.records.tabs')}>
        {(['live', 'deleted'] as const).map((k) => (
          <button
            key={k}
            type="button"
            className={`tt-button${tab === k ? ' tt-primary' : ''}`}
            aria-pressed={tab === k}
            onClick={() => choose(k)}
          >
            {t(`apps.records.${k}`)}
          </button>
        ))}
      </nav>
      {(list.error || error) && (
        <p role="alert" className="tt-error">
          {error ?? list.error}
        </p>
      )}
      {notice && (
        <p role="status" className="tt-muted tt-small">
          {notice}
        </p>
      )}
      {!list.data && !list.error && <p className="tt-muted">{t('app.loading')}</p>}
      {list.data?.records.length === 0 && (
        <p className="tt-muted">{t(tab === 'deleted' ? 'apps.records.noneDeleted' : 'apps.records.none')}</p>
      )}
      {list.data && list.data.records.length > 0 && (
        <ul className="tt-record-list">
          {list.data.records.map((rec) => (
            <li key={rec.id} className="tt-record">
              <div className="tt-record-head">
                <strong>{t('apps.records.record', { id: String(rec.id) })}</strong>
                <span className="tt-muted tt-small">
                  {t('apps.records.revision', { revision: String(rec.revision) })}
                </span>
              </div>
              <div className="tt-record-data">{preview(rec.data, f, t)}</div>
              <RecordPhotos base={base} data={rec.data} />
              <div className="tt-muted tt-small">
                {rec.deleted_at
                  ? t('apps.records.deletedBy', { when: f.dateTime(rec.deleted_at), name: who(rec.deleted_by, t) })
                  : t('apps.records.changed', { when: f.dateTime(rec.updated_at), name: who(rec.updated_by, t) })}
              </div>
              <div className="tt-record-actions">
                <button
                  type="button"
                  className="tt-button"
                  aria-expanded={history === rec.id}
                  aria-label={t(history === rec.id ? 'apps.records.hideHistory' : 'apps.records.historyNamed', {
                    id: String(rec.id),
                  })}
                  onClick={() => setHistory(history === rec.id ? null : rec.id)}
                >
                  {history === rec.id ? t('apps.records.hideHistory') : t('apps.records.history')}
                </button>
                {tab === 'deleted' && canRestore && (
                  <button
                    type="button"
                    className="tt-button tt-primary"
                    disabled={busy !== null}
                    aria-label={t('apps.records.restoreNamed', { id: String(rec.id) })}
                    onClick={() => restore(rec)}
                  >
                    {t('apps.records.restore')}
                  </button>
                )}
              </div>
              {history === rec.id && <RecordHistory base={base} id={rec.id} />}
            </li>
          ))}
        </ul>
      )}
      {list.data?.truncated && (
        <p className="tt-muted tt-small">
          {t('apps.records.truncated', { count: f.number(list.data.records.length) })}
        </p>
      )}
    </div>
  )
}

function RecordHistory({ base, id }: { base: string; id: number }) {
  const t = useT()
  const f = useFormat()
  const loaded = useLoad(
    () => api<{ history: RecordVersion[]; truncated: boolean }>(`${base}/records/${id}/history`),
    [base, id],
  )
  if (loaded.error) {
    return (
      <p role="alert" className="tt-error">
        {loaded.error}
      </p>
    )
  }
  if (!loaded.data) return <p className="tt-muted">{t('app.loading')}</p>
  const operation = (v: RecordVersion) =>
    t(v.operation === 'restore' ? 'apps.records.restoreOp' : `apps.records.${v.operation}`)
  return (
    <>
      <ol className="tt-history" aria-label={t('apps.records.historyNamed', { id: String(id) })}>
        {loaded.data.history.map((v) => (
          <li key={v.revision}>
            <div className="tt-small">
              {t('apps.records.version', {
                operation: operation(v),
                revision: String(v.revision),
                when: f.dateTime(v.at),
                name: who(v.by, t),
              })}
            </div>
            <div className="tt-muted tt-small tt-record-data">{preview(v.data, f, t)}</div>
            <RecordPhotos base={base} data={v.data} />
            {v.synthetic && <div className="tt-muted tt-small">{t('apps.records.synthetic')}</div>}
          </li>
        ))}
      </ol>
      {loaded.data.truncated && (
        <p className="tt-muted tt-small">
          {t('apps.records.versionsTruncated', { count: f.number(loaded.data.history.length) })}
        </p>
      )}
    </>
  )
}

// The thumbnails of the photos a record's attachment fields hold, field by field.
function RecordPhotos({ base, data }: { base: string; data: Record<string, unknown> }) {
  return (
    <>
      {Object.entries(data).map(([name, v]) => {
        const ids = photoIds(v)
        return ids.length > 0 && ids.length === (v as unknown[]).length ? (
          <PhotoStrip key={name} base={base} ids={ids} label={name} />
        ) : null
      })}
    </>
  )
}

// The name of who did something, and that it was through the agent.
function who(a: RecordAuthor | null, t: T): string {
  if (!a) return t('apps.records.unknownUser')
  return a.agent ? t('apps.records.agent', { name: a.name }) : a.name
}

// A record's fields in one line: "name: value" for each.
function preview(data: Record<string, unknown>, f: Format, t: T): string {
  const parts = Object.entries(data).map(([name, v]) => {
    const photos = photoIds(v)
    const shown =
      typeof v === 'number'
        ? f.number(v)
        : typeof v === 'boolean'
          ? t(v ? 'apps.records.yes' : 'apps.records.no')
          : photos.length > 0 && photos.length === (v as unknown[]).length
            ? t('apps.photos.count', { count: photos.length })
            : String(v)
    return `${name}: ${shown}`
  })
  return parts.length > 0 ? parts.join(' · ') : t('apps.records.empty')
}
