import type { Card } from '../api'
import { type Format, useT } from '../i18n'
import { Heading } from '../ui/Heading'
import { IconButton } from '../ui/IconButton'
import { AddCard } from './AddCard'
import { BoardCard } from './BoardCard'
import type { Column } from './columns'
import type { CardDrag } from './drag'
import { useFocusBack } from './focus'
import { StatusDot } from './StatusDot'

// A column of the board: its status dot, name and count, a button to add a card to it, its
// cards (those the filters show), and the form for a new one. Dropping a card on the column
// puts it last; on a card, before that one. An empty column is a quiet place to drop one.
export function BoardColumn({
  column: col,
  cards: inColumn,
  all,
  url,
  canWrite,
  open,
  filtering,
  today,
  f,
  drag,
  adding,
  onAdding,
  onOpen,
  onAdded,
}: {
  column: Column
  cards: Card[]
  // Every card of the board, for where a move puts one.
  all: Card[]
  url: string
  canWrite: boolean
  open: number | null
  filtering: boolean
  today: string
  f: Format
  drag: CardDrag
  // Whether the new-card form is open here.
  adding: boolean
  onAdding: (adding: boolean) => void
  onOpen: (id: number) => void
  onAdded: (c: Card) => void
}) {
  const t = useT()
  const name = t(col.label)
  // Cancelling the form gives the focus back to the column's add button.
  const addButton = useFocusBack<HTMLButtonElement>(adding)
  return (
    <section
      className="ui-board-column"
      aria-label={name}
      data-column={col.id}
      data-drop={(drag.dragged && drag.over === col.id) || undefined}
      onDragOver={(e) => drag.dragOver(e, col.id)}
      onDrop={(e) => drag.drop(e, col.id)}
    >
      <div className="ui-board-column-head">
        <StatusDot column={col.id} />
        <Heading level={2} size="subsection" className="ui-board-column-name">
          {name} <span className="ui-board-count">{f.number(inColumn.length)}</span>
        </Heading>
        {canWrite && (
          <IconButton
            ref={addButton}
            icon="plus"
            size="sm"
            label={t('boards.addCardTo', { column: name })}
            aria-expanded={adding}
            onClick={() => onAdding(!adding)}
          />
        )}
      </div>
      {inColumn.length > 0 ? (
        <ul className="ui-board-cards">
          {inColumn.map((c) => (
            <BoardCard
              key={c.id}
              card={c}
              all={all}
              shownInColumn={inColumn}
              canWrite={canWrite}
              current={c.id === open}
              dragged={drag.dragged?.id === c.id}
              today={today}
              f={f}
              onDragStart={(e) => drag.start(e, c)}
              onDragEnd={drag.end}
              onDrop={(e) => drag.drop(e, col.id, c)}
              onOpen={() => onOpen(c.id)}
              onMove={(column, before) => drag.move(c, column, before)}
            />
          ))}
        </ul>
      ) : filtering ? (
        <p className="ui-board-empty">{t('boards.noMatches')}</p>
      ) : (
        !adding && <div className="ui-board-empty" />
      )}
      {canWrite && adding && <AddCard url={url} column={col} onAdded={onAdded} onCancel={() => onAdding(false)} />}
    </section>
  )
}
