// Moving a document to another path, naming the version it was based on.

import { type FormEvent, useState } from 'react'
import { api, type Doc } from '../api'
import { useT } from '../i18n'
import { type Change, encodePath } from './docs'
import { Problem } from './Problem'

export function Move({
  base,
  doc,
  onMoved,
  onReload,
  onCancel,
}: {
  base: string
  doc: Doc
  onMoved: (to: string) => void
  onReload: () => void
  onCancel: () => void
}) {
  const [to, setTo] = useState(doc.path)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const target = to.trim().replace(/^\/+/, '')
  const t = useT()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await api<Change>(`${base}/docs/${encodePath(doc.path)}`, {
        method: 'PATCH',
        body: { to: target, base_version: doc.version },
      })
      onMoved(target)
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }
  return (
    <form className="tt-panel tt-form" aria-label={t('knowledge.move.label')} onSubmit={submit}>
      <label>
        {t('knowledge.move.to')}
        <input value={to} onChange={(e) => setTo(e.target.value)} />
      </label>
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
        <button type="submit" className="tt-button tt-primary" disabled={busy || !target || target === doc.path}>
          {t('knowledge.move.submit')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}
