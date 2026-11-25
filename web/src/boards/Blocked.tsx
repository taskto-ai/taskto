import { useState } from 'react'
import type { Card } from '../api'
import { useFormat, useT } from '../i18n'
import { blockedText } from './text'

// Why the card is blocked: a reason, other cards of the board it waits for, or both.
export function Blocked({
  card,
  board,
  canWrite,
  busy,
  onSet,
}: {
  card: Card
  board: Card[]
  canWrite: boolean
  busy: boolean
  onSet: (reason: string | null, by: number[]) => void
}) {
  const [editing, setEditing] = useState(false)
  const [reason, setReason] = useState(card.blocked_reason ?? '')
  const [by, setBy] = useState<number[]>(card.blocked_by)
  const others = board.filter((c) => c.id !== card.id)
  const t = useT()
  const f = useFormat()
  const title = (id: number) => board.find((c) => c.id === id)?.title ?? t('boards.cardNumber', { id: String(id) })

  if (!editing) {
    return (
      <section className="tt-blocked-section" aria-label={t('boards.blocked')}>
        {card.blocked ? (
          <p className="tt-blocked">{blockedText(t, card.blocked_reason, f.list(card.blocked_by.map(title)))}</p>
        ) : (
          <p className="tt-muted">{t('boards.notBlocked')}</p>
        )}
        {canWrite && card.column !== 'archived' && (
          <div className="tt-actions">
            <button type="button" className="tt-button" disabled={busy} onClick={() => setEditing(true)}>
              {card.blocked ? t('boards.changeBlocked') : t('boards.setBlocked')}
            </button>
            {card.blocked && (
              <button type="button" className="tt-button" disabled={busy} onClick={() => onSet(null, [])}>
                {t('boards.unblock')}
              </button>
            )}
          </div>
        )}
      </section>
    )
  }
  return (
    <form
      className="tt-panel tt-form"
      aria-label={t('boards.setBlocked')}
      onSubmit={(e) => {
        e.preventDefault()
        onSet(reason.trim() || null, by)
      }}
    >
      <label>
        {t('boards.reason')}
        <input value={reason} placeholder={t('boards.reasonPlaceholder')} onChange={(e) => setReason(e.target.value)} />
      </label>
      {others.length > 0 && (
        <fieldset className="tt-checks">
          <legend>{t('boards.waitingFor')}</legend>
          {others.map((c) => (
            <label key={c.id}>
              <input
                type="checkbox"
                checked={by.includes(c.id)}
                onChange={(e) => setBy((b) => (e.target.checked ? [...b, c.id] : b.filter((id) => id !== c.id)))}
              />
              {c.title}
            </label>
          ))}
        </fieldset>
      )}
      <div className="tt-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || (!reason.trim() && by.length === 0)}>
          {t('boards.setBlocked')}
        </button>
        <button type="button" className="tt-button" onClick={() => setEditing(false)}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}
