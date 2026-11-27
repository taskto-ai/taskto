import { type CSSProperties, type ReactNode, useEffect, useRef, useState } from 'react'
import { api, type Card, type Space } from '../api'
import { placeholders } from '../byPlace'
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
  openCard?: number
  header: (filters?: ReactNode, add?: ReactNode) => ReactNode
  notices?: ReactNode
}) {
  const cards = useLoad(() => api<{ cards: Card[] }>(url).then((r) => r.cards), [url])
  const [open, setOpen] = useState<number | null>(openCard ?? null)
  const [moved, setMoved] = useState<Card | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [said, setSaid] = useState({ text: '', n: 0 })
  const [adding, setAdding] = useState<Card['column'] | null>(null)
  const focusCard = useRef<number | null>(null)
  const openColumn = useRef<Card['column'] | null>(null)
  const leave = useRef<(() => Promise<boolean>) | null>(null)
  const t = useT()
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
  const [label, setLabel] = useState('')
  const [overdueOnly, setOverdueOnly] = useState(false)
  const today = useToday()
  const f = useFormat()

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
    if (open === id || cards.data.some((c) => c.id === id)) return
    focusCard.current = null
    if (!focusLost()) return
    const column = document.querySelector(`[data-column="${openColumn.current}"]`)
    const board = document.querySelector('.ui-board-columns')
    ;((column && focusables(column)[0]) ?? (board && focusables(board)[0]))?.focus()
  }, [cards.data, open])

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

  const [columns, setColumns] = useState<HTMLDivElement | null>(null)
  useEffect(() => {
    if (!columns) return
    const onFocus = (e: FocusEvent) => {
      if (!(e.target instanceof HTMLElement)) return
      const box = columns.getBoundingClientRect()
      const here = e.target.getBoundingClientRect()
      if (here.left < box.left || here.right > box.right)
        e.target.scrollIntoView?.({ block: 'nearest', inline: 'nearest' })
    }
    columns.addEventListener('focusin', onFocus)
    return () => columns.removeEventListener('focusin', onFocus)
  }, [columns])

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
      <div ref={setColumns} className="ui-board-columns" style={{ '--board-columns': COLUMNS.length } as CSSProperties}>
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
              setAdding((now) => (now === col.id ? null : now))
              cards.reload()
              void show(c.id)
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
          leave={leave}
        />
      )}
    </div>
  )
}

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
            {placeholders(3 - (i % 2)).map(({ key, place: n }) => (
              <Skeleton key={key} shape="block" height={n === 0 ? '72px' : '48px'} />
            ))}
          </div>
        </div>
      ))}
    </div>
  )
}
