import { type FormEvent, useEffect, useRef, useState } from 'react'
import type { Card } from '../api'
import { useFormat, useT } from '../i18n'
import { Icon } from '../shell/Icon'
import type { Fields } from './card'
import { isOverdue, useToday } from './due'

// The card's due date and labels. A due date is a calendar day, so it reads the same wherever
// it is seen; it is late from the day after, for a card that isn't done. Labels are free text
// the server normalizes (Unicode form, case, spaces), so what people type is sent as it is and
// the card shown is the server's. A label is added or removed over the latest card's, so
// redoing the change keeps the labels others set. The due date is a draft until it is saved
// (with Enter or the button), so typing a date a part at a time never saves the part.
export function Planning({
  card,
  canWrite,
  busy,
  onChange,
}: {
  card: Card
  canWrite: boolean
  busy: boolean
  onChange: (fields: Fields) => Promise<boolean>
}) {
  const [due, setDue] = useState(card.due ?? '')
  const [label, setLabel] = useState('')
  // The draft follows the card's date when that changes (a save, or someone else's edit),
  // unless it has been edited since: a date typed while a save was pending stays to be saved.
  const synced = useRef(card.due ?? '')
  useEffect(() => {
    const next = card.due ?? ''
    const previous = synced.current
    synced.current = next
    setDue((draft) => (draft === previous ? next : draft))
  }, [card.due])
  const t = useT()
  const f = useFormat()
  const editable = canWrite && card.column !== 'archived'
  const today = useToday()
  const overdue = isOverdue(card, today)
  const typed = label.trim()
  const add = async (e: FormEvent) => {
    e.preventDefault()
    if (!typed) return
    // The server normalizes and removes repeats; the field empties once it has saved.
    // Only if it still holds what was sent: a label typed meanwhile stays.
    const sent = label
    if (await onChange((base) => ({ labels: [...base.labels, typed] }))) setLabel((now) => (now === sent ? '' : now))
  }
  const saveDue = (e: FormEvent) => {
    e.preventDefault()
    // Emptying a part of the field gives '': clearing is the button's.
    if (due && due !== card.due) void onChange({ due })
  }
  return (
    <>
      <div className="tt-planning">
        {editable ? (
          <form className="tt-actions" onSubmit={saveDue}>
            <label className="tt-inline-field">
              {t('boards.dueDate')}
              <input
                type="date"
                value={due}
                data-overdue={overdue || undefined}
                onChange={(e) => setDue(e.target.value)}
              />
            </label>
            <button type="submit" className="tt-button" disabled={busy || !due || due === card.due}>
              {t('boards.saveDue')}
            </button>
            {card.due && (
              <button type="button" className="tt-button" disabled={busy} onClick={() => onChange({ due: null })}>
                {t('boards.clearDue')}
              </button>
            )}
          </form>
        ) : card.due ? (
          <p className="tt-due" data-overdue={overdue || undefined}>
            {t(overdue ? 'boards.overdueOn' : 'boards.dueOn', { date: f.calendarDate(card.due) })}
          </p>
        ) : (
          <p className="tt-muted">{t('boards.noDue')}</p>
        )}
      </div>
      <section className="tt-planning" aria-label={t('boards.labels')}>
        {card.labels.length > 0 ? (
          <ul className="tt-labels">
            {card.labels.map((l) => (
              <li key={l} className="tt-label">
                {l}
                {editable && (
                  <button
                    type="button"
                    className="tt-icon-button"
                    aria-label={t('boards.removeLabel', { label: l })}
                    disabled={busy}
                    onClick={() => onChange((base) => ({ labels: base.labels.filter((x) => x !== l) }))}
                  >
                    <Icon name="close" />
                  </button>
                )}
              </li>
            ))}
          </ul>
        ) : (
          <p className="tt-muted">{t('boards.noLabels')}</p>
        )}
        {editable && (
          <form className="tt-actions" onSubmit={add}>
            <input
              aria-label={t('boards.labelField')}
              value={label}
              maxLength={30}
              placeholder={t('boards.labelPlaceholder')}
              onChange={(e) => setLabel(e.target.value)}
            />
            <button type="submit" className="tt-button" disabled={busy || !typed}>
              {t('boards.addLabel')}
            </button>
          </form>
        )}
      </section>
    </>
  )
}
