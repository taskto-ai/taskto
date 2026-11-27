import { type DragEvent, useState } from 'react'
import { ApiError, api, type Card, errorText } from '../api'
import { useT } from '../i18n'
import { dropPosition } from './move'

export function useCardDrag({
  url,
  cards,
  onMoved,
  onError,
  reload,
}: {
  url: string
  cards: Card[] | undefined
  onMoved: (c: Card, place: MovedTo) => void
  onError: (error: string | null) => void
  reload: () => void
}) {
  const [dragged, setDragged] = useState<Card | null>(null)
  const [over, setOver] = useState<Card['column'] | null>(null)
  const t = useT()

  const move = async (c: Card, column: Card['column'], before?: Card): Promise<boolean> => {
    const position = dropPosition(cards ?? [], c, column, before)
    if (position === null) return false
    onError(null)
    try {
      const updated = await api<Card>(`${url}/${c.id}/move`, {
        method: 'POST',
        body: { version: c.version, column, ...(before ? { position } : {}) },
      })
      onMoved(updated, { before, position })
    } catch (err) {
      onError(
        err instanceof ApiError && err.status === 409 ? t('boards.cardMoved', { title: c.title }) : errorText(err),
      )
    }
    reload()
    return true
  }

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

export type MovedTo = { before?: Card; position: number }
