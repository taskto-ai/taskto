import { type FormEvent, useState } from 'react'
import { api, type Card, errorText } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Input } from '../ui/Input'
import type { Column } from './columns'

export function AddCard({
  url,
  column,
  onAdded,
  onCancel,
}: {
  url: string
  column: Column
  onAdded: (c: Card) => void
  onCancel: () => void
}) {
  const [title, setTitle] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const t = useT()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const card = await api<Card>(url, { method: 'POST', body: { title: title.trim(), column: column.id } })
      setTitle('')
      onAdded(card)
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form
      className="ui-board-add"
      aria-label={t('boards.newCardIn', { column: t(column.label) })}
      onSubmit={submit}
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          e.preventDefault()
          onCancel()
        }
      }}
    >
      <Input
        aria-label={t('boards.cardTitle')}
        value={title}
        maxLength={200}
        invalid={Boolean(error)}
        onChange={(e) => setTitle(e.target.value)}
        autoFocus
      />
      {error && (
        <p role="alert" className="ui-board-error">
          {error}
        </p>
      )}
      <div className="ui-board-actions">
        <Button type="submit" size="sm" loading={busy} disabled={!title.trim()}>
          {t('boards.add')}
        </Button>
        <Button size="sm" variant="ghost" onClick={onCancel}>
          {t('common.cancel')}
        </Button>
      </div>
    </form>
  )
}
