import { type FormEvent, useEffect, useRef, useState } from 'react'
import type { Card } from '../api'
import { useFormat, useT } from '../i18n'
import { Button } from '../ui/Button'
import { IconButton } from '../ui/IconButton'
import { Input } from '../ui/Input'
import { Label } from '../ui/Label'
import { DueBadge, dueWords } from './BoardCard'
import type { Fields } from './card'
import { useDraft } from './drafts'
import { dueState, useToday } from './due'

export function Due({
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
  const synced = useRef(card.due ?? '')
  useEffect(() => {
    const next = card.due ?? ''
    const previous = synced.current
    synced.current = next
    setDue((draft) => (draft === previous ? next : draft))
  }, [card.due])
  const t = useT()
  const f = useFormat()
  const today = useToday()
  const state = dueState(card, today)
  useDraft(canWrite && due !== '' && due !== (card.due ?? ''))
  const badge = state && card.due && <DueBadge state={state} text={dueWords(t, state, card.due, today, f)} />
  if (!canWrite || card.column === 'archived')
    return badge || <span className="ui-board-muted">{t('boards.noDue')}</span>

  const saveDue = (e: FormEvent) => {
    e.preventDefault()
    if (due && due !== card.due) void onChange({ due })
  }
  return (
    <form className="ui-board-inline" onSubmit={saveDue}>
      <Input
        type="date"
        size="sm"
        aria-label={t('boards.dueDate')}
        value={due}
        onChange={(e) => setDue(e.target.value)}
        className="ui-board-date"
      />
      {due && due !== card.due && (
        <Button type="submit" size="sm" disabled={busy} aria-label={t('boards.saveDue')}>
          {t('boards.save')}
        </Button>
      )}
      {card.due && (
        <IconButton
          icon="close"
          size="sm"
          label={t('boards.clearDue')}
          disabled={busy}
          onClick={() => onChange({ due: null })}
        />
      )}
      {state !== 'later' && badge}
    </form>
  )
}

export function Labels({
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
  const [adding, setAdding] = useState(false)
  const [label, setLabel] = useState('')
  const t = useT()
  const editable = canWrite && card.column !== 'archived'
  const typed = label.trim()
  useDraft(adding && typed !== '')
  const add = async (e: FormEvent) => {
    e.preventDefault()
    if (!typed) return
    const sent = label
    if (await onChange((base) => ({ labels: [...base.labels, typed] }))) setLabel((now) => (now === sent ? '' : now))
  }
  return (
    <div className="ui-board-labels">
      {card.labels.length === 0 && !adding && <span className="ui-board-muted">{t('boards.noLabels')}</span>}
      {card.labels.map((l) => (
        <span key={l} className="ui-board-label">
          <Label name={l} />
          {editable && (
            <IconButton
              icon="close"
              size="sm"
              label={t('boards.removeLabel', { label: l })}
              disabled={busy}
              onClick={() => onChange((base) => ({ labels: base.labels.filter((x) => x !== l) }))}
            />
          )}
        </span>
      ))}
      {editable && !adding && (
        <IconButton
          icon="plus"
          size="sm"
          label={t('boards.newLabel')}
          data-focus-default
          onClick={() => setAdding(true)}
        />
      )}
      {editable && adding && (
        <form
          className="ui-board-inline"
          aria-label={t('boards.newLabel')}
          onSubmit={add}
          onKeyDown={(e) => {
            if (e.key === 'Escape') {
              e.preventDefault()
              setAdding(false)
            }
          }}
        >
          <Input
            size="sm"
            className="ui-board-grow"
            aria-label={t('boards.labelField')}
            value={label}
            maxLength={30}
            onChange={(e) => setLabel(e.target.value)}
            autoFocus
          />
          <Button type="submit" size="sm" disabled={busy || !typed}>
            {t('boards.addLabel')}
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setAdding(false)}>
            {t('common.cancel')}
          </Button>
        </form>
      )}
    </div>
  )
}
