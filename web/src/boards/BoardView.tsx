import { type CSSProperties, type ReactNode, useEffect, useRef, useState } from 'react'
import { api, type Card, type Space } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Button } from '../ui/Button'
import { focusables } from '../ui/layers'
import { Notice } from '../ui/Notice'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { BoardColumn } from './BoardColumn'
import { BoardFilters } from './BoardFilters'
import { CardView } from './CardView'
import { COLUMN_LABELS, COLUMNS } from './columns'
import { type MovedTo, useCardDrag } from './drag'
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
  // What a screen reader is told after a move; `n` makes a repeated message a new one.
  const [said, setSaid] = useState({ text: '', n: 0 })
  // The column whose new-card form is open, if any.
  const [adding, setAdding] = useState<Card['column'] | null>(null)
  // The card to give the focus to once the board is drawn again: one that moved (and was drawn
  // anew in its column), or the one whose drawer closed. Only when the focus has nowhere else to be.
  const focusCard = useRef<number | null>(null)
  // The open card's column as last seen on the board: where the focus goes when its drawer closes
  // after it left the board (archived).
  const openColumn = useRef<Card['column'] | null>(null)
  // The open card's question before it goes (CardView): it may hold a draft.
  const leave = useRef<(() => Promise<boolean>) | null>(null)
  const t = useT()
  // Where a move put a card, in words: the top of a column, before a card, or the bottom.
  const movedText = (c: Card, place: MovedTo) => {
    const column = t(COLUMN_LABELS[c.column])
    if (!place.before) return t('boards.movedBottom', { title: c.title, column })
    if (place.position === 0) return t('boards.movedTop', { title: c.title, column })
    return t('boards.movedBefore', { title: c.title, before: place.before.title, column })
  }
  const drag = useCardDrag({
    url,
    cards: cards.data,
    onMoved: (updated, place) => {
      if (updated.id === open) setMoved(updated)
      focusCard.current = updated.id
      setSaid((was) => ({ text: movedText(updated, place), n: was.n + 1 }))
    },
    onError: setError,
    reload: cards.reload,
  })
  // The board's filters, applied to the cards already loaded: a label, and only the overdue.
  const [label, setLabel] = useState('')
  const [overdueOnly, setOverdueOnly] = useState(false)
  const today = useToday()
  const f = useFormat()

  // Opens a card, once the open one (if another) may go.
  const show = async (id: number) => {
    if (open !== null && open !== id && leave.current && !(await leave.current())) return
    setOpen(id)
  }

  useEffect(() => {
    const id = focusCard.current
    if (id === null || !cards.data) return
    const button = document.querySelector<HTMLElement>(`[data-card-open="${id}"]`)
    if (button) {
      focusCard.current = null
      if (focusLost()) button.focus()
      return
    }
    // The card is no longer on the board (archived): the focus goes to the column it was in, or
    // else to the board's first card.
    if (open === id || cards.data.some((c) => c.id === id)) return
    focusCard.current = null
    if (!focusLost()) return
    const column = document.querySelector(`[data-column="${openColumn.current}"]`)
    const board = document.querySelector('.ui-board-columns')
    ;((column && focusables(column)[0]) ?? (board && focusables(board)[0]))?.focus()
  }, [cards.data, open])

  // The open card's own tile, brought into view beside the drawer (the columns' scroll-padding keeps
  // it out from under the drawer, boards.css), once per opening: when it opens, or when the board's
  // cards arrive for a card opened from its address.
  const shownTile = useRef<number | null>(null)
  useEffect(() => {
    if (open === null) {
      shownTile.current = null
      return
    }
    if (shownTile.current === open || !cards.data) return
    const tile = document.querySelector(`[data-card-open="${open}"]`)
    if (!tile) return
    shownTile.current = open
    tile.scrollIntoView?.({ block: 'nearest', inline: 'nearest' })
  }, [open, cards.data])

  // The same element around the board whatever it shows, so its head isn't drawn anew (and its
  // menu closed) when the cards arrive.
  if (cards.error && !cards.data)
    return (
      <div className="ui-board">
        {header()}
        {notices}
        <Notice tone="danger">{cards.error}</Notice>
      </div>
    )
  if (!cards.data)
    return (
      <div className="ui-board">
        {header()}
        {notices}
        <BoardSkeleton />
      </div>
    )

  const { allLabels, filtering, shown, filterable } = boardFilter(cards.data, label, overdueOnly, today)
  const all = cards.data
  if (open !== null) openColumn.current = all.find((c) => c.id === open)?.column ?? openColumn.current

  return (
    <div className="ui-board" data-drawer={open !== null || undefined}>
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
        <VisuallyHidden key={said.n}>{said.text}</VisuallyHidden>
      </div>
      <div className="ui-board-columns" style={{ '--board-columns': COLUMNS.length } as CSSProperties}>
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
            onOpen={show}
            onAdded={(c) => {
              // Only this column's form: another opened while this card was on its way stays.
              setAdding((now) => (now === col.id ? null : now))
              cards.reload()
              void show(c.id)
            }}
          />
        ))}
        {/* With a card open on a wide board, room as wide as the drawer after the last column, so
            the board's sideways scroll brings that column out from under the card. */}
        {open !== null && <div className="ui-board-drawer-room" aria-hidden="true" />}
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
          leave={leave}
        />
      )}
    </div>
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
