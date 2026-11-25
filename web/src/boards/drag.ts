import { type DragEvent, useState } from 'react'
import { ApiError, api, type Card, errorText } from '../api'
import { useT } from '../i18n'
import { dropPosition } from './move'

// Dragging a card: which one, and the column it is over. Each card's Move menu does the same
// from the keyboard and on touch, so dragging is only a quicker way for a mouse. A drop (or the
// menu) moves the card with the version last read; onMoved gets the card as moved, and onError the
// error to show (null when a move starts). The board is read again after every move.
export function useCardDrag({
  url,
  cards,
  onMoved,
  onError,
  reload,
}: {
  url: string
  cards: Card[] | undefined
  onMoved: (c: Card) => void
  onError: (error: string | null) => void
  reload: () => void
}) {
  const [dragged, setDragged] = useState<Card | null>(null)
  const [over, setOver] = useState<Card['column'] | null>(null)
  const t = useT()

  // Moves a card to `column`, before `before` or last; position counts the column's cards without
  // it, as the server does. Answers whether a move was sent: none when it would leave the card
  // where it is. A drop and the card's Move menu both move through it.
  const move = async (c: Card, column: Card['column'], before?: Card): Promise<boolean> => {
    const position = dropPosition(cards ?? [], c, column, before)
    if (position === null) return false
    onError(null)
    try {
      const updated = await api<Card>(`${url}/${c.id}/move`, {
        method: 'POST',
        body: { version: c.version, column, ...(before ? { position } : {}) },
      })
      onMoved(updated)
    } catch (err) {
      onError(
        err instanceof ApiError && err.status === 409 ? t('boards.cardMoved', { title: c.title }) : errorText(err),
      )
    }
    reload()
    return true
  }

  // Drops the dragged card in `column`, before `before` or last.
  const drop = async (e: DragEvent, column: Card['column'], before?: Card) => {
    e.preventDefault()
    e.stopPropagation()
    const c = dragged
    setDragged(null)
    setOver(null)
    if (!c) return
    await move(c, column, before)
  }
  const dragOver = (e: DragEvent, column: Card['column']) => {
    if (!dragged) return
    e.preventDefault()
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'move'
    setOver(column)
  }
  const start = (e: DragEvent, c: Card) => {
    // Some browsers start a drag only with data set.
    e.dataTransfer?.setData('text/plain', c.title)
    if (e.dataTransfer) e.dataTransfer.effectAllowed = 'move'
    setDragged(c)
  }
  const end = () => {
    setDragged(null)
    setOver(null)
  }
  return { dragged, over, drop, dragOver, start, end, move }
}

export type CardDrag = ReturnType<typeof useCardDrag>
