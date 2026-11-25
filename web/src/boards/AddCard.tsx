import { type FormEvent, useState } from 'react'
import { api, type Card, errorText } from '../api'
import { useT } from '../i18n'
import { Icon } from '../shell/Icon'
import type { Column } from './columns'

// Adds a card to a column: a button, which opens a form for its title.
export function AddCard({ url, column, onAdded }: { url: string; column: Column; onAdded: (c: Card) => void }) {
  const [open, setOpen] = useState(false)
  const [title, setTitle] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const t = useT()
  if (!open) {
    return (
      <button
        type="button"
        className="tt-button tt-add-card"
        aria-label={t('boards.addCardTo', { column: t(column.label) })}
        onClick={() => setOpen(true)}
      >
        <Icon name="plus" />
        {t('boards.addCard')}
      </button>
    )
  }
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const card = await api<Card>(url, { method: 'POST', body: { title: title.trim(), column: column.id } })
      setTitle('')
      setOpen(false)
      onAdded(card)
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form className="tt-form" aria-label={t('boards.newCardIn', { column: t(column.label) })} onSubmit={submit}>
      <input
        aria-label={t('boards.cardTitle')}
        value={title}
        maxLength={200}
        onChange={(e) => setTitle(e.target.value)}
        // biome-ignore lint/a11y/noAutofocus: the form was opened to type this
        autoFocus
      />
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      <div className="tt-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || !title.trim()}>
          {t('boards.add')}
        </button>
        <button type="button" className="tt-button" onClick={() => setOpen(false)}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}
