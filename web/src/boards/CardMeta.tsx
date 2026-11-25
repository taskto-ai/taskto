import type { Card } from '../api'
import { useT } from '../i18n'
import { Assignee } from './Assignee'
import { COLUMN_LABELS, COLUMNS } from './columns'
import type { Person } from './people'
import { assigneeLabel } from './text'

// The card's column, its assignee, and archiving or restoring it. The Column field moves the
// card from the keyboard, as dragging it does on the board.
export function CardMeta({
  card,
  canWrite,
  people,
  busy,
  onMove,
  onAssign,
  onArchive,
  onRestore,
}: {
  card: Card
  canWrite: boolean
  people: Person[] | undefined
  busy: boolean
  onMove: (column: string) => void
  onAssign: (assignee: Card['assignee']) => void
  onArchive: () => void
  onRestore: () => void
}) {
  const t = useT()
  return (
    <div className="tt-card-meta">
      {canWrite && card.column !== 'archived' ? (
        <label className="tt-inline-field">
          {t('boards.column')}
          <select value={card.column} disabled={busy} onChange={(e) => onMove(e.target.value)}>
            {COLUMNS.map((c) => (
              <option key={c.id} value={c.id}>
                {t(c.label)}
              </option>
            ))}
          </select>
        </label>
      ) : (
        <span className="tt-muted">{t(COLUMN_LABELS[card.column])}</span>
      )}
      {canWrite ? (
        <Assignee card={card} people={people} busy={busy} onSet={onAssign} />
      ) : (
        card.assignee && (
          <span className="tt-muted">{t('boards.assignedTo', { name: assigneeLabel(t, card.assignee) })}</span>
        )
      )}
      {canWrite &&
        (card.column === 'archived' ? (
          <button type="button" className="tt-button" disabled={busy} onClick={onRestore}>
            {t('boards.restore')}
          </button>
        ) : (
          <button type="button" className="tt-button" disabled={busy} onClick={onArchive}>
            {t('boards.archive')}
          </button>
        ))}
    </div>
  )
}
