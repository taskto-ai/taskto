import { useState } from 'react'
import { api, type Card, type Space } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { BoardColumn } from './BoardColumn'
import { BoardFilters } from './BoardFilters'
import { CardView } from './CardView'
import { COLUMNS } from './columns'
import { useCardDrag } from './drag'
import { useToday } from './due'
import { boardFilter } from './filters'

// A board: its cards by column, narrowed by its filters, and the open card beside them (on a
// phone, in their place).
export function BoardView({
  url,
  workspace,
  space,
  canWrite,
  openCard,
}: {
  url: string
  workspace: number
  space: Space
  canWrite: boolean
  // The card to open on, a link's.
  openCard?: number
}) {
  const cards = useLoad(() => api<{ cards: Card[] }>(url).then((r) => r.cards), [url])
  const [open, setOpen] = useState<number | null>(openCard ?? null)
  // The open card as a drag moved it, for the card view to show.
  const [moved, setMoved] = useState<Card | null>(null)
  const [error, setError] = useState<string | null>(null)
  const drag = useCardDrag({
    url,
    cards: cards.data,
    onMoved: (updated) => {
      if (updated.id === open) setMoved(updated)
    },
    onError: setError,
    reload: cards.reload,
  })
  // The board's filters, applied to the cards already loaded: a label, and only the overdue.
  const [label, setLabel] = useState('')
  const [overdueOnly, setOverdueOnly] = useState(false)
  const today = useToday()
  const t = useT()
  const f = useFormat()
  if (cards.error && !cards.data)
    return (
      <p role="alert" className="tt-error">
        {cards.error}
      </p>
    )
  if (!cards.data) return <p className="tt-muted">{t('app.loading')}</p>

  const { allLabels, filtering, shown, filterable } = boardFilter(cards.data, label, overdueOnly, today)

  return (
    <div className="tt-board" data-open={open ? 'card' : 'board'}>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      {filterable && (
        <BoardFilters
          labels={allLabels}
          label={label}
          onLabel={setLabel}
          overdueOnly={overdueOnly}
          onOverdueOnly={setOverdueOnly}
        />
      )}
      <div className="tt-columns">
        {COLUMNS.map((col) => (
          <BoardColumn
            key={col.id}
            column={col}
            cards={cards.data?.filter((c) => c.column === col.id && shown(c)) ?? []}
            url={url}
            canWrite={canWrite}
            open={open}
            filtering={filtering}
            today={today}
            f={f}
            drag={drag}
            onOpen={setOpen}
            onAdded={(c) => {
              cards.reload()
              setOpen(c.id)
            }}
          />
        ))}
      </div>
      {open && (
        <CardView
          key={open}
          url={`${url}/${open}`}
          workspace={workspace}
          space={space}
          board={cards.data}
          canWrite={canWrite}
          moved={moved?.id === open ? moved : null}
          onChanged={cards.reload}
          onClose={() => setOpen(null)}
        />
      )}
    </div>
  )
}
