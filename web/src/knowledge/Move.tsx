// Moving a document to another path, in a dialog, naming the version it was based on.

import { type FormEvent, useId, useState } from 'react'
import { api, type Doc } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Dialog } from '../ui/Dialog'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
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
  const form = useId()
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
    <Dialog
      open
      title={t('knowledge.move.label')}
      onClose={onCancel}
      actions={
        <>
          <Button onClick={onCancel}>{t('common.cancel')}</Button>
          <Button
            type="submit"
            form={form}
            variant="primary"
            loading={busy}
            disabled={busy || !target || target === doc.path}
          >
            {t('knowledge.move.submit')}
          </Button>
        </>
      }
    >
      <form id={form} className="kb-form" aria-label={t('knowledge.move.label')} onSubmit={submit}>
        <Field label={t('knowledge.move.to')}>
          <Input data-autofocus value={to} onChange={(e) => setTo(e.target.value)} />
        </Field>
        {error !== null && (
          <Problem
            error={error}
            onReload={() => {
              setError(null)
              onReload()
            }}
          />
        )}
      </form>
    </Dialog>
  )
}
