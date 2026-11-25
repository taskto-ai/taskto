// Deleting a document, after a question, and the notice after it, with the way to bring it
// back from its history.

import { useState } from 'react'
import { api, type Doc, errorText } from '../api'
import { useT } from '../i18n'
import { type Change, type Commit, encodePath, useMounted } from './docs'
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
  onChanged,
}: {
  base: string
  path: string
  versioned: boolean
  canWrite: boolean
  // The document is back: it opens, if the notice is still there.
  onRestored: (path: string) => void
  // The space's list of documents changed, whatever became of the notice.
  onChanged: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const t = useT()
  // A restore the person asked for is finished even if they have moved on (opened another
  // document, say); only opening the document afterwards is for someone still here.
  const mounted = useMounted()
  const restore = async () => {
    setBusy(true)
    setError(null)
    try {
      // The newest version still kept, however far back the history goes: a page at a time.
      const history = `${base}/history/${encodePath(path)}`
      let before: Commit | undefined
      for (let page = '', first = true; !before; first = false) {
        const res = await api<{ commits: Commit[]; next?: string }>(
          page ? `${history}?before=${encodeURIComponent(page)}` : history,
        )
        // Someone made it again since: nothing to bring back, it is there.
        if (first && res.commits[0] && res.commits[0].change !== 'delete') {
          onChanged()
          if (mounted.current) onRestored(path)
          return
        }
        before = res.commits.find((c) => c.change !== 'delete' && c.versioned !== false)
        if (!before && !res.next) throw new Error(t('knowledge.deleted.noEarlier', { path }))
        page = res.next ?? ''
      }
      await api<Change>(`${base}/restore/${encodePath(path)}`, { method: 'POST', body: { commit: before.commit } })
      onChanged()
      if (mounted.current) onRestored(path)
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
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
