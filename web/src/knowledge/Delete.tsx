// Deleting a document, after a question, and the notice after it, with the way to bring it
// back from its history.

import { useEffect, useRef, useState } from 'react'
import { api, type Doc, errorText } from '../api'
import { useT } from '../i18n'
import { type Change, type Commit, encodePath } from './docs'
import { Problem } from './Problem'

export function ConfirmDelete({
  url,
  doc,
  onDeleted,
  onReload,
  onCancel,
}: {
  url: string
  doc: Doc
  onDeleted: () => void
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
      await api<Change>(url, { method: 'DELETE', body: { base_version: doc.version } })
      onDeleted()
    } catch (err) {
      setError(err)
      setBusy(false)
    }
  }
  return (
    <fieldset className="tt-panel" aria-label={t('knowledge.delete.label')}>
      <p>
        {t(doc.versioned === false ? 'knowledge.delete.questionUnversioned' : 'knowledge.delete.question', {
          path: doc.path,
        })}
      </p>
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
          {t('knowledge.delete.submit')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </fieldset>
  )
}

// After a delete: the document can come back as it was, from its history.
export function Deleted({
  base,
  path,
  versioned,
  canWrite,
  onRestored,
}: {
  base: string
  path: string
  versioned: boolean
  canWrite: boolean
  onRestored: (path: string) => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const t = useT()
  // The search of the history stops when the notice goes away.
  const search = useRef<AbortController | null>(null)
  useEffect(() => () => search.current?.abort(), [])
  const restore = async () => {
    setBusy(true)
    setError(null)
    search.current = new AbortController()
    const { signal } = search.current
    try {
      // The newest version still kept, however far back the history goes: a page at a time.
      const history = `${base}/history/${encodePath(path)}`
      let before: Commit | undefined
      for (let page = '', first = true; !before; first = false) {
        const res = await api<{ commits: Commit[]; next?: string }>(
          page ? `${history}?before=${encodeURIComponent(page)}` : history,
          { signal },
        )
        // Someone made it again since: nothing to bring back, it is there.
        if (first && res.commits[0] && res.commits[0].change !== 'delete') return onRestored(path)
        before = res.commits.find((c) => c.change !== 'delete' && c.versioned !== false)
        if (!before && !res.next) throw new Error(t('knowledge.deleted.noEarlier', { path }))
        page = res.next ?? ''
      }
      // The notice went away while the history was searched, or the restore was on its way: the
      // person is elsewhere by now, and it is not opened over what they are reading.
      if (signal.aborted) return
      await api<Change>(`${base}/restore/${encodePath(path)}`, { method: 'POST', body: { commit: before.commit } })
      if (!signal.aborted) onRestored(path)
    } catch (err) {
      if (!signal.aborted) setError(errorText(err))
    } finally {
      if (!signal.aborted) setBusy(false)
    }
  }
  if (!versioned) {
    return (
      <div className="tt-notice" role="status">
        <span>{t('knowledge.deleted.unversioned', { path })}</span>
      </div>
    )
  }
  return (
    <div className="tt-notice" role="status">
      <span>{t(canWrite ? 'knowledge.deleted.restorable' : 'knowledge.deleted.kept', { path })}</span>
      {canWrite && (
        <button type="button" className="tt-button" onClick={restore} disabled={busy}>
          {t('knowledge.deleted.restore')}
        </button>
      )}
      {error && <span className="tt-error">{error}</span>}
    </div>
  )
}
