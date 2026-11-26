// Deleting a document, after a question, and the notice after it, with the way to bring it
// back from its history.

import { useState } from 'react'
import { api, type Doc, errorText } from '../api'
import { useT } from '../i18n'
import { steps } from '../session'
import { Button } from '../ui/Button'
import { ConfirmDialog } from '../ui/ConfirmDialog'
import { Notice } from '../ui/Notice'
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
    <ConfirmDialog
      open
      title={t('knowledge.delete.label')}
      confirmLabel={t('knowledge.delete.submit')}
      tone="danger"
      busy={busy}
      onConfirm={remove}
      onCancel={onCancel}
    >
      <div className="kb-dialog-text">
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
      </div>
    </ConfirmDialog>
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
  // document, say); only opening the document afterwards is for someone still here. Its steps go
  // on only in the session that asked (session.ts): after a sign-out, or someone else's sign-in,
  // nothing more is read, restored or said.
  const mounted = useMounted()
  const restore = async () => {
    setBusy(true)
    setError(null)
    const s = steps()
    try {
      // The newest version still kept, however far back the history goes: a page at a time.
      const history = `${base}/history/${encodePath(path)}`
      let before: Commit | undefined
      for (let page = '', first = true; !before; first = false) {
        const res = s.check(
          await api<{ commits: Commit[]; next?: string }>(
            page ? `${history}?before=${encodeURIComponent(page)}` : history,
          ),
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
      s.check(
        await api<Change>(`${base}/restore/${encodePath(path)}`, { method: 'POST', body: { commit: before.commit } }),
      )
      onChanged()
      if (mounted.current) onRestored(path)
    } catch (err) {
      if (s.ok() && mounted.current) setError(errorText(err))
    } finally {
      if (mounted.current) setBusy(false)
    }
  }
  if (!versioned) {
    // Neutral (it can't come back, and that is not an error), and said when it appears.
    return (
      <div role="status">
        <Notice tone="neutral">{t('knowledge.deleted.unversioned', { path })}</Notice>
      </div>
    )
  }
  return (
    <Notice
      tone="info"
      action={
        canWrite && (
          <Button size="sm" onClick={restore} loading={busy}>
            {t('knowledge.deleted.restore')}
          </Button>
        )
      }
    >
      {t(canWrite ? 'knowledge.deleted.restorable' : 'knowledge.deleted.kept', { path })}
      {error && <span className="kb-error">{error}</span>}
    </Notice>
  )
}
