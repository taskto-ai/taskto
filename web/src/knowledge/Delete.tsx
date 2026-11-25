// Deleting a document, after a question, and the notice after it, with the way to bring it
// back from its history.

import { useState } from 'react'
import { api, type Doc, errorText } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { ConfirmDialog } from '../ui/ConfirmDialog'
import { Notice } from '../ui/Notice'
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
  const restore = async () => {
    setBusy(true)
    setError(null)
    try {
      const { commits } = await api<{ commits: Commit[] }>(`${base}/history/${encodePath(path)}?limit=10`)
      // Someone made it again since: nothing to bring back, it is there.
      if (commits[0] && commits[0].change !== 'delete') return onRestored(path)
      const before = commits.find((c) => c.change !== 'delete')
      if (!before) throw new Error(t('knowledge.deleted.noEarlier', { path }))
      await api<Change>(`${base}/restore/${encodePath(path)}`, { method: 'POST', body: { commit: before.commit } })
      onRestored(path)
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }
  if (!versioned) {
    return <Notice tone="neutral">{t('knowledge.deleted.unversioned', { path })}</Notice>
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
