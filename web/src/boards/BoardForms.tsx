import { type FormEvent, useId, useState } from 'react'
import { api, type Board, errorText } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Dialog } from '../ui/Dialog'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'

// A board's name, asked in a dialog: renaming one, or a new one.
function NameDialog({
  title,
  formLabel,
  initial,
  submitLabel,
  error,
  onSubmit,
  onCancel,
}: {
  title: string
  formLabel: string
  initial: string
  submitLabel: string
  error?: string | null
  onSubmit: (name: string) => Promise<void>
  onCancel: () => void
}) {
  const [name, setName] = useState(initial)
  const [busy, setBusy] = useState(false)
  const form = useId()
  const t = useT()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    await onSubmit(name.trim())
    setBusy(false)
  }
  return (
    <Dialog
      open
      onClose={onCancel}
      title={title}
      size="sm"
      actions={
        <>
          <Button onClick={onCancel}>{t('common.cancel')}</Button>
          <Button type="submit" form={form} variant="primary" loading={busy} disabled={!name.trim()}>
            {submitLabel}
          </Button>
        </>
      }
    >
      <form id={form} aria-label={formLabel} onSubmit={submit}>
        <Field label={t('boards.name')} error={error ?? undefined}>
          <Input value={name} maxLength={100} onChange={(e) => setName(e.target.value)} data-autofocus />
        </Field>
      </form>
    </Dialog>
  )
}

export function RenameBoard({
  board,
  error,
  onSave,
  onCancel,
}: {
  board: Board
  // Why the last save failed.
  error: string | null
  onSave: (name: string) => Promise<void>
  onCancel: () => void
}) {
  const t = useT()
  return (
    <NameDialog
      title={t('boards.renameBoard')}
      formLabel={t('boards.renameBoard')}
      initial={board.name}
      submitLabel={t('boards.save')}
      error={error}
      onSubmit={onSave}
      onCancel={onCancel}
    />
  )
}

export function NewBoard({
  url,
  onCreated,
  onCancel,
}: {
  url: string
  onCreated: (b: Board) => void
  onCancel: () => void
}) {
  const [error, setError] = useState<string | null>(null)
  const t = useT()
  const create = async (name: string) => {
    setError(null)
    try {
      onCreated(await api<Board>(url, { method: 'POST', body: { name } }))
    } catch (err) {
      setError(errorText(err))
    }
  }
  return (
    <NameDialog
      title={t('boards.newBoard')}
      formLabel={t('boards.newBoard')}
      initial=""
      submitLabel={t('boards.create')}
      error={error}
      onSubmit={create}
      onCancel={onCancel}
    />
  )
}
