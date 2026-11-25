import { type FormEvent, useState } from 'react'
import { api, type Board, errorText } from '../api'
import { useT } from '../i18n'

export function RenameBoard({
  board,
  onSave,
  onCancel,
}: {
  board: Board
  onSave: (name: string) => Promise<void>
  onCancel: () => void
}) {
  const [name, setName] = useState(board.name)
  const [busy, setBusy] = useState(false)
  const t = useT()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    await onSave(name.trim())
    setBusy(false)
  }
  return (
    <form className="tt-panel tt-form" aria-label={t('boards.renameBoard')} onSubmit={submit}>
      <label>
        {t('boards.name')}
        <input
          value={name}
          maxLength={100}
          onChange={(e) => setName(e.target.value)}
          // biome-ignore lint/a11y/noAutofocus: the form was opened to type this
          autoFocus
        />
      </label>
      <div className="tt-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || !name.trim()}>
          {t('boards.save')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
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
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const t = useT()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      onCreated(await api<Board>(url, { method: 'POST', body: { name: name.trim() } }))
    } catch (err) {
      setError(errorText(err))
      setBusy(false)
    }
  }
  return (
    <form className="tt-panel tt-form" aria-label={t('boards.newBoard')} onSubmit={submit}>
      <label>
        {t('boards.name')}
        <input
          value={name}
          maxLength={100}
          onChange={(e) => setName(e.target.value)}
          // biome-ignore lint/a11y/noAutofocus: the form was opened to type this
          autoFocus
        />
      </label>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      <div className="tt-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || !name.trim()}>
          {t('boards.create')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}
