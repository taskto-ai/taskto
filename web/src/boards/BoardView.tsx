import { type ReactNode, useEffect, useRef, useState } from 'react'
import { api, type Card, type Space } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Button } from '../ui/Button'
import { Notice } from '../ui/Notice'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { BoardColumn } from './BoardColumn'
import { BoardFilters } from './BoardFilters'
import { CardView } from './CardView'
import { COLUMN_LABELS, COLUMNS } from './columns'
import { useCardDrag } from './drag'
import { useToday } from './due'
import { boardFilter } from './filters'
import { focusLost } from './focus'

// A board: the page's head with the board's filters and Add a card, its cards by column (the
// columns scroll sideways in their own box), and the open card in a drawer over them.
export function BoardView({
  url,
  workspace,
  space,
  canWrite,
  openCard,
  header,
  notices,
}: {
  url: string
  workspace: number
  space: Space
  canWrite: boolean
  // The card to open on, a link's.
  openCard?: number
  // The page's head, given the board's filters and its Add a card.
  header: (filters?: ReactNode, add?: ReactNode) => ReactNode
  // The page's notices, under the head.
  notices?: ReactNode
}) {
  const cards = useLoad(() => api<{ cards: Card[] }>(url).then((r) => r.cards), [url])
  const [open, setOpen] = useState<number | null>(openCard ?? null)
  // The open card as a drag moved it, for the card view to show.
  const [moved, setMoved] = useState<Card | null>(null)
  const [error, setError] = useState<string | null>(null)
  // What a screen reader is told after a move.
  const [said, setSaid] = useState('')
  // The column whose new-card form is open, if any.
  const [adding, setAdding] = useState<Card['column'] | null>(null)
  // The card to give the focus to once the board is drawn again: one that moved (and was drawn
  // anew in its column), or the one whose drawer closed. Only when the focus has nowhere else to be.
  const focusCard = useRef<number | null>(null)
  const t = useT()
  const drag = useCardDrag({
    url,
    cards: cards.data,
    onMoved: (updated) => {
      if (updated.id === open) setMoved(updated)
      focusCard.current = updated.id
      setSaid(t('boards.movedTo', { title: updated.title, column: t(COLUMN_LABELS[updated.column]) }))
    },
    onError: setError,
    reload: cards.reload,
  })
  // The board's filters, applied to the cards already loaded: a label, and only the overdue.
  const [label, setLabel] = useState('')
  const [overdueOnly, setOverdueOnly] = useState(false)
  const today = useToday()
  const f = useFormat()

  // biome-ignore lint/correctness/useExhaustiveDependencies: run again when the cards or the open card change
  useEffect(() => {
    const id = focusCard.current
    if (id === null || open === id) return
    const button = document.querySelector<HTMLElement>(`[data-card-open="${id}"]`)
    if (!button) return
    focusCard.current = null
    if (focusLost()) button.focus()
  }, [cards.data, open])

  if (cards.error && !cards.data)
    return (
      <>
        {header()}
        {notices}
        <Notice tone="danger">{cards.error}</Notice>
      </>
    )
  if (!cards.data)
    return (
      <>
        {header()}
        {notices}
        <BoardSkeleton />
      </>
    )

  const { allLabels, filtering, shown, filterable } = boardFilter(cards.data, label, overdueOnly, today)
  const all = cards.data

  return (
    <>
      {header(
        filterable && (
          <BoardFilters
            labels={allLabels}
            label={label}
            onLabel={setLabel}
            overdueOnly={overdueOnly}
            onOverdueOnly={setOverdueOnly}
          />
        ),
        canWrite && (
          <Button variant="primary" icon="plus" onClick={() => setAdding(COLUMNS[0].id)}>
            {t('boards.addCard')}
          </Button>
        ),
      )}
      {notices}
      {error && <Notice tone="danger">{error}</Notice>}
      <div role="status" className="ui-board-said">
        <VisuallyHidden>{said}</VisuallyHidden>
      </div>
      <div className="ui-board-columns">
        {COLUMNS.map((col) => (
          <BoardColumn
            key={col.id}
            column={col}
            cards={all.filter((c) => c.column === col.id && shown(c))}
            all={all}
            url={url}
            canWrite={canWrite}
            open={open}
            filtering={filtering}
            today={today}
            f={f}
            drag={drag}
            adding={adding === col.id}
            onAdding={(on) => setAdding(on ? col.id : null)}
            onOpen={setOpen}
            onAdded={(c) => {
              setAdding(null)
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
          board={all}
          canWrite={canWrite}
          moved={moved?.id === open ? moved : null}
          onChanged={cards.reload}
          onClose={() => {
            focusCard.current = open
            setOpen(null)
          }}
        />
      )}
    </>
  )
}

// The board's shape while its cards load: columns with a few cards in each.
export function BoardSkeleton() {
  const t = useT()
  return (
    <div className="ui-board-columns" aria-busy="true">
      <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
      {COLUMNS.map((col, i) => (
        <div key={col.id} className="ui-board-column">
          <div className="ui-board-column-head">
            <Skeleton width="45%" />
          </div>
          <div className="ui-board-cards">
            {Array.from({ length: 3 - (i % 2) }, (_, n) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: placeholders with nothing else to tell them apart
              <Skeleton key={n} shape="block" height={n === 0 ? '72px' : '48px'} />
            ))}
          </div>
        </div>
      ))}
    </div>
  )
}
