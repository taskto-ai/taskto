import type { Card } from '../api'
import { useFormat, useT } from '../i18n'
import { AddCard } from './AddCard'
import { BoardCard } from './BoardCard'
import type { Column } from './columns'
import type { CardDrag } from './drag'

// A column of the board: its name and count, its cards (those the filters show), and adding
// one. Dropping a card on the column puts it last; on a card, before that one.
export function BoardColumn({
  column: col,
  cards: inColumn,
  url,
  canWrite,
  open,
  filtering,
  today,
  drag,
  onOpen,
  onAdded,
}: {
  column: Column
  cards: Card[]
  url: string
  canWrite: boolean
  open: number | null
  filtering: boolean
  today: string
  drag: CardDrag
  onOpen: (id: number) => void
  onAdded: (c: Card) => void
}) {
  const t = useT()
  const f = useFormat()
  return (
    <section
      className="tt-column"
      aria-label={t(col.label)}
      data-drop={(drag.dragged && drag.over === col.id) || undefined}
      onDragOver={(e) => drag.dragOver(e, col.id)}
      onDrop={(e) => drag.drop(e, col.id)}
    >
      <h2>
        {t(col.label)} <span className="tt-muted">{f.number(inColumn.length)}</span>
      </h2>
      <ul>
        {inColumn.map((c) => (
          <BoardCard
            key={c.id}
            card={c}
            canWrite={canWrite}
            current={c.id === open}
            dragged={drag.dragged?.id === c.id}
            today={today}
            onDragStart={(e) => drag.start(e, c)}
            onDragEnd={drag.end}
            onDrop={(e) => drag.drop(e, col.id, c)}
            onOpen={() => onOpen(c.id)}
          />
        ))}
      </ul>
      {filtering && inColumn.length === 0 && <p className="tt-muted tt-small">{t('boards.noMatches')}</p>}
      {canWrite && <AddCard url={url} column={col} onAdded={onAdded} />}
    </section>
  )
}
