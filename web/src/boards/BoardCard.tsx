import type { DragEvent } from 'react'
import type { Card } from '../api'
import { useFormat, useT } from '../i18n'
import { isOverdue } from './due'
import { assigneeLabel } from './text'

// A card on the board: a button that opens it, draggable by a writer, and a place to drop
// another card before it.
export function BoardCard({
  card: c,
  canWrite,
  current,
  dragged,
  today,
  onDragStart,
  onDragEnd,
  onDrop,
  onOpen,
}: {
  card: Card
  canWrite: boolean
  // Whether it is the open card.
  current: boolean
  dragged: boolean
  today: string
  onDragStart: (e: DragEvent) => void
  onDragEnd: () => void
  onDrop: (e: DragEvent) => void
  onOpen: () => void
}) {
  const t = useT()
  const f = useFormat()
  return (
    <li
      draggable={canWrite}
      data-dragged={dragged || undefined}
      onDragStart={(e) => {
        if (!canWrite) return
        onDragStart(e)
      }}
      onDragEnd={onDragEnd}
      onDrop={onDrop}
    >
      <button
        type="button"
        className="tt-card"
        data-blocked={c.blocked || undefined}
        aria-current={current || undefined}
        onClick={onOpen}
      >
        <span>{c.title}</span>
        {c.due && (
          <span className="tt-due tt-small" data-overdue={isOverdue(c, today) || undefined}>
            {t(isOverdue(c, today) ? 'boards.overdueOn' : 'boards.dueOn', {
              date: f.calendarDate(c.due),
            })}
          </span>
        )}
        {c.labels.length > 0 && (
          <span className="tt-labels">
            {c.labels.map((l) => (
              <span key={l} className="tt-label tt-small">
                {l}
              </span>
            ))}
          </span>
        )}
        {c.blocked && <span className="tt-blocked tt-small">{t('boards.blocked')}</span>}
        {c.assignee && <span className="tt-muted tt-small">{assigneeLabel(t, c.assignee)}</span>}
      </button>
    </li>
  )
}
