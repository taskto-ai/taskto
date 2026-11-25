import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { api, type Doc } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import type { LinkHandler } from '../markdown'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { ConfirmDialog, useConfirm } from '../ui/ConfirmDialog'
import { Heading } from '../ui/Heading'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { Skeleton } from '../ui/Skeleton'
import { Content } from './Content'
import { type Change, type Commit, encodePath, isStale, useDoc, useLatest } from './docs'
import { useLoadingStatus } from './loading'
import { Problem } from './Problem'

const CHANGE_LABEL = {
  create: 'knowledge.history.create',
  update: 'knowledge.history.update',
  delete: 'knowledge.history.delete',
} as const

// The commits that changed a document, newest first; any version can be looked at, and
// writers can make the document that version again, after a question. An unversioned file's
// history only says who changed it: its older versions' bytes aren't kept, nor those of
// versions removed from history, which an admin can do.
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
  const [confirmDialog, confirm] = useConfirm()
  useLoadingStatus(!first.data && !first.error)
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
  const askRestore = async (c: Commit) => {
    const time = f.dateTime(c.time)
    if (
      await confirm({
        title: t('knowledge.history.restoreTitle'),
        body: t('knowledge.history.restoreQuestion', { path: current.path, time }),
        confirmLabel: t('knowledge.history.restore'),
      })
    )
      restore(c)
  }

  return (
    <section className="kb-history" aria-label={t('knowledge.history.label')}>
      {first.error && (
        <Notice
          tone="danger"
          title={t('knowledge.errors.history')}
          action={
            <Button size="sm" onClick={first.reload}>
              {t('app.tryAgain')}
            </Button>
          }
        >
          {first.error}
        </Notice>
      )}
      {!first.data && !first.error && (
        <div className="kb-history-loading">
          {['70%', '50%', '60%'].map((width, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: placeholders have no identity but their place
            <Skeleton key={i} width={width} />
          ))}
        </div>
      )}
      {unversioned && <Notice tone="neutral">{t('knowledge.history.unversioned')}</Notice>}
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
      {commits.length > 0 && (
        <Panel>
          <Rows label={t('knowledge.history.label')}>
            {commits.map((c) => {
              const isCurrent = !!c.version && c.version === current.version
              const viewable = !!c.version && !unversioned && (kept(c) || isCurrent)
              const by = c.agent
                ? t('knowledge.history.byWithAgent', { name: c.author.name, agent: c.agent, time: f.dateTime(c.time) })
                : t('knowledge.history.by', { name: c.author.name, time: f.dateTime(c.time) })
              const removedVersion = !!c.version && !unversioned && !kept(c) && !isCurrent
              return (
                <Row
                  key={c.commit}
                  current={c === viewing}
                  title={c.message || t(c.change ? CHANGE_LABEL[c.change] : 'knowledge.history.changed')}
                  detail={
                    <>
                      {t('knowledge.history.changeBy', {
                        change: t(c.change ? CHANGE_LABEL[c.change] : 'knowledge.history.changed'),
                        by,
                      })}
                      {removedVersion && (
                        <>
                          <br />
                          {t('knowledge.history.removedVersion')}
                        </>
                      )}
                    </>
                  }
                  status={isCurrent ? <Badge tone="success">{t('knowledge.history.current')}</Badge> : undefined}
                  actions={
                    viewable ? (
                      <>
                        <Button
                          size="sm"
                          variant="ghost"
                          aria-pressed={c === viewing}
                          onClick={() => setViewing(c === viewing ? null : c)}
                        >
                          {t('knowledge.history.view')}
                        </Button>
                        {!isCurrent && canWrite && (
                          <Button size="sm" disabled={busy} onClick={() => askRestore(c)}>
                            {t('knowledge.history.restore')}
                          </Button>
                        )}
                      </>
                    ) : undefined
                  }
                />
              )
            })}
          </Rows>
        </Panel>
      )}
      {(next || canRemove) && (
        <div className="kb-actions">
          {next && (
            <Button size="sm" loading={loadingMore} onClick={more}>
              {t('knowledge.history.older')}
            </Button>
          )}
          {canRemove && (
            <Button size="sm" variant="danger" className="kb-push-end" onClick={() => setRemoving(true)}>
              {t('knowledge.history.removeOld')}
            </Button>
          )}
        </div>
      )}
      {removed !== null && (
        <Notice tone="success">
          {removed === 0 ? t('knowledge.history.removedNone') : t('knowledge.history.removedCount', { count: removed })}
        </Notice>
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
      {confirmDialog}
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
    <ConfirmDialog
      open
      title={t('knowledge.history.removeLabel')}
      confirmLabel={t('knowledge.history.removeSubmit')}
      tone="danger"
      busy={busy}
      onConfirm={remove}
      onCancel={onCancel}
    >
      <div className="kb-dialog-text">
        <p>{t('knowledge.history.removeQuestion', { path: doc.path })}</p>
        <p>{t('knowledge.history.removeForGood')}</p>
        <p>{t('knowledge.history.removeBackups')}</p>
        {error !== null && (
          <Problem
            error={error}
            onReload={() => {
              setError(null)
              onReload()
            }}
          />
        )}
      </div>
    </ConfirmDialog>
  )
}

// A version as it was. Opening one moves the keyboard to it.
function AtCommit({ url, commit, onLink }: { url: string; commit: Commit; onLink: LinkHandler }) {
  const at = `${url}?at=${commit.commit}`
  const doc = useDoc(at)
  useLoadingStatus(!doc.data && !doc.error)
  const t = useT()
  const f = useFormat()
  const asOf = t('knowledge.history.asOf', { time: f.dateTime(commit.time) })
  const section = useRef<HTMLElement>(null)
  useLayoutEffect(() => {
    section.current?.querySelector<HTMLElement>('h3')?.focus()
  }, [])
  return (
    <section className="kb-version" aria-label={asOf} ref={section}>
      <Heading level={3} size="subsection" tabIndex={-1}>
        {asOf}
      </Heading>
      {doc.error && <Notice tone="danger">{doc.error}</Notice>}
      {!doc.data && !doc.error && (
        <div className="kb-history-loading">
          <Skeleton width="80%" />
          <Skeleton width="60%" />
        </div>
      )}
      {doc.data && <Content url={at} doc={doc.data} onLink={onLink} />}
    </section>
  )
}
