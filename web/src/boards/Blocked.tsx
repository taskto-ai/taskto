import { useState } from 'react'
import type { Card } from '../api'
import { useFormat, useT } from '../i18n'
import { Button } from '../ui/Button'
import { Checkbox } from '../ui/Checkbox'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { useDraft } from './drafts'
import { blockedText } from './text'

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
  useDraft(
    editing &&
      (reason.trim() !== (card.blocked_reason ?? '').trim() ||
        [...by].sort().join() !== [...card.blocked_by].sort().join()),
  )
  const others = board.filter((c) => c.id !== card.id)
  const t = useT()
  const f = useFormat()
  const title = (id: number) => board.find((c) => c.id === id)?.title ?? t('boards.cardNumber', { id: String(id) })

  if (!editing) {
    return (
      <div className="ui-board-blocked">
        {card.blocked ? (
          <span className="ui-board-blocked-text">
            {blockedText(t, card.blocked_reason, f.list(card.blocked_by.map(title)))}
          </span>
        ) : (
          <span className="ui-board-muted">{t('boards.notBlocked')}</span>
        )}
        {canWrite && card.column !== 'archived' && (
          <span className="ui-board-actions">
            <Button size="sm" variant="ghost" disabled={busy} onClick={() => setEditing(true)}>
              {card.blocked ? t('boards.changeBlocked') : t('boards.setBlocked')}
            </Button>
            {card.blocked && (
              <Button size="sm" variant="ghost" disabled={busy} onClick={() => onSet(null, [])}>
                {t('boards.unblock')}
              </Button>
            )}
          </span>
        )}
      </div>
    )
  }
  return (
    <form
      className="ui-board-form"
      aria-label={t('boards.setBlocked')}
      onSubmit={(e) => {
        e.preventDefault()
        onSet(reason.trim() || null, by)
      }}
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          e.preventDefault()
          setEditing(false)
        }
      }}
    >
      <Field label={t('boards.reason')}>
        <Input size="sm" value={reason} onChange={(e) => setReason(e.target.value)} autoFocus />
      </Field>
      {others.length > 0 && (
        <fieldset className="ui-board-checks">
          <legend className="ui-board-checks-legend">{t('boards.waitingFor')}</legend>
          {others.map((c) => (
            <Checkbox
              key={c.id}
              label={<span className="ui-board-check-title">{c.title}</span>}
              checked={by.includes(c.id)}
              onChange={(e) => setBy((b) => (e.target.checked ? [...b, c.id] : b.filter((id) => id !== c.id)))}
            />
          ))}
        </fieldset>
      )}
      <div className="ui-board-actions">
        <Button type="submit" size="sm" disabled={busy || (!reason.trim() && by.length === 0)}>
          {t('boards.setBlocked')}
        </Button>
        <Button size="sm" variant="ghost" onClick={() => setEditing(false)}>
          {t('common.cancel')}
        </Button>
      </div>
    </form>
  )
}
