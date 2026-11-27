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
import { authorName, type Change, type Commit, encodePath, isMarkdown, isStale, useDoc, useLatest } from './docs'
import { useLoadingStatus } from './loading'
import { Problem } from './Problem'
import { type DiffAgainst, VersionDiff } from './VersionDiff'

type Page = { commits: Commit[]; next?: string }

type Open = { commit: Commit; mode: 'view' } | { commit: Commit; mode: 'diff'; against: DiffAgainst }

const CHANGE_LABEL = {
  create: 'knowledge.history.create',
  update: 'knowledge.history.update',
  delete: 'knowledge.history.delete',
} as const

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
  const [generation, setGeneration] = useState(0)
  const first = useLoad(() => api<Page>(historyUrl), [historyUrl, generation])
  const [removing, setRemoving] = useState(false)
  const [removed, setRemoved] = useState<number | null>(null)
  const [older, setOlder] = useState<Page[]>([])
  const [open, setOpen] = useState<Open | null>(null)
  const [finding, setFinding] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<{ error: unknown; reload: () => void; reloadable?: boolean } | null>(null)
  const [confirmDialog, confirm] = useConfirm()
  useLoadingStatus(!first.data && !first.error)
  const section = useRef<HTMLElement>(null)
  const unversioned = current.versioned === false
  const pages = first.data ? [first.data, ...older] : []
  const commits = pages.flatMap((p) => p.commits)
  const next = pages.at(-1)?.next
  const kept = (c: Commit) => c.versioned !== false
  const canRemove =
    admin &&
    first.data !== undefined &&
    (next !== undefined || commits.some((c) => c.version && c.version !== current.version && kept(c)))
  const t = useT()
  const f = useFormat()

  const loadingOlder = useRef<AbortController | null>(null)
  const [loadingMore, setLoadingMore] = useState(false)
  const now = useLatest(generation)
  useEffect(() => () => loadingOlder.current?.abort(), [])
  const fetchOlder = async (token: string): Promise<Page | null> => {
    if (loadingOlder.current) return null
    const made = generation
    const request = new AbortController()
    loadingOlder.current = request
    setLoadingMore(true)
    try {
      const page = await api<Page>(`${historyUrl}?before=${encodeURIComponent(token)}`, {
        signal: request.signal,
      })
      if (request.signal.aborted || now.current !== made) return null
      setOlder((o) => [...o, page])
      return page
    } catch (err) {
      if (request.signal.aborted || now.current !== made) return null
      setError({ error: err, reload: reloadHistory, reloadable: isStale(err) })
      return null
    } finally {
      if (loadingOlder.current === request) {
        loadingOlder.current = null
        setLoadingMore(false)
      }
    }
  }
  const more = async () => {
    if (next) await fetchOlder(next)
  }
  const readable = (c: Commit) => !!c.version && !unversioned && kept(c)
  const previousOf = async (c: Commit): Promise<Commit | null | 'removed' | 'failed'> => {
    let list = commits
    let token = next
    for (;;) {
      const older = list.slice(list.findIndex((x) => x.commit === c.commit) + 1)
      const found = older.find(readable)
      if (found) return found
      if (older.some((x) => !!x.version)) return 'removed'
      if (!token) return null
      const page = await fetchOlder(token)
      if (!page) return 'failed'
      list = [...list, ...page.commits]
      token = page.next
    }
  }
  const showChanges = async (c: Commit) => {
    if (open?.commit === c && open.mode === 'diff' && open.against.kind === 'previous') {
      setOpen(null)
      return
    }
    setNotice(null)
    setFinding(c.commit)
    const made = generation
    const previous = c.change === 'create' ? null : await previousOf(c)
    if (now.current !== made) return
    setFinding(null)
    if (previous === 'failed') return
    if (previous === 'removed') {
      setOpen(null)
      setNotice(t('knowledge.history.previousRemoved'))
      return
    }
    setOpen({ commit: c, mode: 'diff', against: { kind: 'previous', commit: previous } })
  }
  const toggleCompare = (c: Commit) => {
    setNotice(null)
    const same = open?.commit === c && open.mode === 'diff' && open.against.kind === 'current'
    setOpen(same ? null : { commit: c, mode: 'diff', against: { kind: 'current' } })
  }
  const toggleView = (c: Commit) => {
    setNotice(null)
    setOpen(open?.commit === c && open.mode === 'view' ? null : { commit: c, mode: 'view' })
  }
  const reloadHistory = () => {
    loadingOlder.current?.abort()
    loadingOlder.current = null
    setLoadingMore(false)
    setOpen(null)
    setFinding(null)
    setNotice(null)
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
    <section className="kb-history" aria-label={t('knowledge.history.label')} ref={section} tabIndex={-1}>
      {first.error && (
        <Notice
          tone="danger"
          title={t('knowledge.errors.history')}
          action={
            <Button
              size="sm"
              onClick={() => {
                section.current?.focus()
                first.reload()
              }}
            >
              {t('app.tryAgain')}
            </Button>
          }
        >
          {first.error}
        </Notice>
      )}
      {!first.data && !first.error && (
        <div className="kb-history-loading">
          {['70%', '50%', '60%'].map((width) => (
            <Skeleton key={width} width={width} />
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
              const diffable = viewable && isMarkdown(current.path)
              const by = c.agent
                ? t('knowledge.history.byWithAgent', {
                    name: authorName(c.author, t),
                    agent: c.agent,
                    time: f.dateTime(c.time),
                  })
                : t('knowledge.history.by', { name: authorName(c.author, t), time: f.dateTime(c.time) })
              const removedVersion = !!c.version && !unversioned && !kept(c) && !isCurrent
              return (
                <Row
                  key={c.commit}
                  current={c === open?.commit}
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
                          aria-pressed={c === open?.commit && open.mode === 'view'}
                          onClick={() => toggleView(c)}
                        >
                          {t('knowledge.history.view')}
                        </Button>
                        {diffable && (
                          <Button
                            size="sm"
                            variant="ghost"
                            loading={finding === c.commit}
                            aria-pressed={
                              c === open?.commit && open.mode === 'diff' && open.against.kind === 'previous'
                            }
                            onClick={() => showChanges(c)}
                          >
                            {t('knowledge.history.changes')}
                          </Button>
                        )}
                        {diffable && !isCurrent && (
                          <Button
                            size="sm"
                            variant="ghost"
                            aria-pressed={c === open?.commit && open.mode === 'diff' && open.against.kind === 'current'}
                            onClick={() => toggleCompare(c)}
                          >
                            {t('knowledge.history.compareCurrent')}
                          </Button>
                        )}
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
            reloadHistory()
          }}
          onReload={onReload}
          onCancel={() => setRemoving(false)}
        />
      )}
      {notice && <Notice tone="neutral">{notice}</Notice>}
      {open?.mode === 'view' && <AtCommit key={open.commit.commit} url={url} commit={open.commit} onLink={onLink} />}
      {open?.mode === 'diff' && (
        <VersionDiff
          key={`${open.commit.commit}:${open.against.kind}`}
          url={url}
          commit={open.commit}
          against={open.against}
          restoring={busy}
          onRestore={
            canWrite && !(!!open.commit.version && open.commit.version === current.version)
              ? () => askRestore(open.commit)
              : undefined
          }
        />
      )}
      {confirmDialog}
    </section>
  )
}

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
