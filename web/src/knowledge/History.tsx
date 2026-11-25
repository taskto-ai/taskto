import { useEffect, useRef, useState } from 'react'
import { api, type Doc } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import type { LinkHandler } from '../markdown'
import { Content } from './Content'
import { type Change, type Commit, encodePath, isStale, useDoc, useLatest } from './docs'
import { Problem } from './Problem'

const CHANGE_LABEL = {
  create: 'knowledge.history.create',
  update: 'knowledge.history.update',
  delete: 'knowledge.history.delete',
} as const

// The commits that changed a document, newest first; any version can be looked at, and
// writers can make the document that version again. An unversioned file's history only says
// who changed it: its older versions' bytes aren't kept, nor those of versions removed from
// history, which an admin can do.
export function History({
  base,
  url,
  current,
  canWrite,
  admin,
  onLink,
  onRestored,
  onReload,
}: {
  base: string
  url: string
  current: Doc
  canWrite: boolean
  admin: boolean
  onLink: LinkHandler
  onRestored: () => void
  onReload: () => void
}) {
  const historyUrl = `${base}/history/${encodePath(current.path)}`
  // Each time the list is made again (old versions removed, a reload) it is a new generation:
  // the old one's pages, request and views are gone, and a late answer for it is dropped.
  const [generation, setGeneration] = useState(0)
  const first = useLoad(() => api<{ commits: Commit[]; next?: string }>(historyUrl), [historyUrl, generation])
  const [removing, setRemoving] = useState(false)
  const [removed, setRemoved] = useState<number | null>(null)
  const [older, setOlder] = useState<{ commits: Commit[]; next?: string }[]>([])
  const [viewing, setViewing] = useState<Commit | null>(null)
  const [busy, setBusy] = useState(false)
  // What failed, and what its Reload brings back: the older page's reloads the history, a
  // restore's reloads the document and the history.
  const [error, setError] = useState<{ error: unknown; reload: () => void; reloadable?: boolean } | null>(null)
  const unversioned = current.versioned === false
  const pages = first.data ? [first.data, ...older] : []
  const commits = pages.flatMap((p) => p.commits)
  const next = pages.at(-1)?.next
  const kept = (c: Commit) => c.versioned !== false
  // Something to remove: an older version still kept, or one not loaded yet.
  const canRemove =
    admin &&
    first.data !== undefined &&
    (next !== undefined || commits.some((c) => c.version && c.version !== current.version && kept(c)))
  const t = useT()
  const f = useFormat()

  // One page at a time: a second click while one loads would add the same page twice.
  const loadingOlder = useRef<AbortController | null>(null)
  const [loadingMore, setLoadingMore] = useState(false)
  const now = useLatest(generation)
  useEffect(() => () => loadingOlder.current?.abort(), [])
  const more = async () => {
    if (!next || loadingOlder.current) return
    const made = generation
    const request = new AbortController()
    loadingOlder.current = request
    setLoadingMore(true)
    try {
      const page = await api<{ commits: Commit[]; next?: string }>(`${historyUrl}?before=${encodeURIComponent(next)}`, {
        signal: request.signal,
      })
      if (!request.signal.aborted && now.current === made) setOlder((o) => [...o, page])
    } catch (err) {
      // A request this list ended itself (it was made again, or went away) is not an error.
      if (request.signal.aborted || now.current !== made) return
      setError({ error: err, reload: reloadHistory, reloadable: isStale(err) })
    } finally {
      if (loadingOlder.current === request) {
        loadingOlder.current = null
        setLoadingMore(false)
      }
    }
  }
  // The list again from its newest page: what it showed, and asked for, is dropped.
  const reloadHistory = () => {
    loadingOlder.current?.abort()
    loadingOlder.current = null
    setLoadingMore(false)
    setViewing(null)
    setOlder([])
    setError(null)
    setGeneration((g) => g + 1)
  }
  const restore = async (c: Commit) => {
    setBusy(true)
    setError(null)
    try {
      await api<Change>(`${base}/restore/${encodePath(current.path)}`, {
        method: 'POST',
        body: { commit: c.commit, base_version: current.version },
      })
      onRestored()
    } catch (err) {
      setError({
        error: err,
        reload: () => {
          reloadHistory()
          onReload()
        },
      })
      setBusy(false)
    }
  }

  return (
    <section className="tt-history" aria-label={t('knowledge.history.label')}>
      {first.error && (
        <p role="alert" className="tt-error">
          {first.error}
        </p>
      )}
      {!first.data && !first.error && <p className="tt-muted">{t('app.loading')}</p>}
      {unversioned && <p className="tt-muted tt-small">{t('knowledge.history.unversioned')}</p>}
      {error !== null && (
        <Problem
          error={error.error}
          reloadable={error.reloadable}
          onReload={() => {
            setError(null)
            error.reload()
          }}
        />
      )}
      <ol className="tt-list">
        {commits.map((c) => (
          <li key={c.commit} className="tt-history-entry" aria-current={c === viewing || undefined}>
            <span>
              <strong>{t(c.change ? CHANGE_LABEL[c.change] : 'knowledge.history.changed')}</strong> {c.message}
            </span>
            <span className="tt-muted tt-small">
              {c.agent
                ? t('knowledge.history.byWithAgent', { name: c.author.name, agent: c.agent, time: f.dateTime(c.time) })
                : t('knowledge.history.by', { name: c.author.name, time: f.dateTime(c.time) })}
            </span>
            {c.version && unversioned && c.version === current.version && (
              <span className="tt-muted tt-small">{t('knowledge.history.current')}</span>
            )}
            {c.version && !unversioned && !kept(c) && c.version !== current.version && (
              <span className="tt-muted tt-small">{t('knowledge.history.removedVersion')}</span>
            )}
            {c.version && !unversioned && (kept(c) || c.version === current.version) && (
              <span className="tt-actions">
                <button type="button" className="tt-button" onClick={() => setViewing(c)}>
                  {t('knowledge.history.view')}
                </button>
                {c.version === current.version ? (
                  <span className="tt-muted tt-small">{t('knowledge.history.current')}</span>
                ) : (
                  canWrite && (
                    <button type="button" className="tt-button" disabled={busy} onClick={() => restore(c)}>
                      {t('knowledge.history.restore')}
                    </button>
                  )
                )}
              </span>
            )}
          </li>
        ))}
      </ol>
      {next && (
        <button type="button" className="tt-button" disabled={loadingMore} onClick={more}>
          {t('knowledge.history.older')}
        </button>
      )}
      {removed !== null && (
        <p role="status" className="tt-muted tt-small">
          {removed === 0 ? t('knowledge.history.removedNone') : t('knowledge.history.removedCount', { count: removed })}
        </p>
      )}
      {canRemove && !removing && (
        <button type="button" className="tt-button tt-danger" onClick={() => setRemoving(true)}>
          {t('knowledge.history.removeOld')}
        </button>
      )}
      {removing && (
        <ConfirmRemoveOld
          url={historyUrl}
          doc={current}
          onRemoved={(n) => {
            setRemoving(false)
            setRemoved(n)
            // The commits have new IDs: the history is loaded again.
            reloadHistory()
          }}
          onReload={onReload}
          onCancel={() => setRemoving(false)}
        />
      )}
      {viewing && <AtCommit key={viewing.commit} url={url} commit={viewing} onLink={onLink} />}
    </section>
  )
}

// Removing a file's older versions from history, for an admin: it can't be undone, and
// the backups made before still hold them, so the question says both.
function ConfirmRemoveOld({
  url,
  doc,
  onRemoved,
  onReload,
  onCancel,
}: {
  url: string
  doc: Doc
  onRemoved: (count: number) => void
  onReload: () => void
  onCancel: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const t = useT()
  const remove = async () => {
    setBusy(true)
    setError(null)
    try {
      const res = await api<{ commit: string; removed: number }>(url, {
        method: 'DELETE',
        body: { base_version: doc.version },
      })
      onRemoved(res.removed)
    } catch (err) {
      setError(err)
      setBusy(false)
    }
  }
  return (
    <fieldset className="tt-panel" aria-label={t('knowledge.history.removeLabel')}>
      <p>{t('knowledge.history.removeQuestion', { path: doc.path })}</p>
      <p className="tt-small">{t('knowledge.history.removeForGood')}</p>
      <p className="tt-small">{t('knowledge.history.removeBackups')}</p>
      {error !== null && (
        <Problem
          error={error}
          onReload={() => {
            setError(null)
            onReload()
          }}
        />
      )}
      <div className="tt-actions">
        <button type="button" className="tt-button tt-danger" onClick={remove} disabled={busy}>
          {t('knowledge.history.removeSubmit')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </fieldset>
  )
}

function AtCommit({ url, commit, onLink }: { url: string; commit: Commit; onLink: LinkHandler }) {
  const at = `${url}?at=${commit.commit}`
  const doc = useDoc(at)
  const t = useT()
  const f = useFormat()
  const asOf = t('knowledge.history.asOf', { time: f.dateTime(commit.time) })
  return (
    <section className="tt-preview" aria-label={asOf}>
      <p className="tt-muted tt-small">{asOf}</p>
      {doc.error && (
        <p role="alert" className="tt-error">
          {doc.error}
        </p>
      )}
      {doc.data && <Content url={at} doc={doc.data} onLink={onLink} />}
    </section>
  )
}
