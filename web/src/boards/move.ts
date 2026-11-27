import type { Card } from '../api'

export function dropPosition(cards: Card[], card: Card, column: Card['column'], before?: Card): number | null {
  if (before?.id === card.id) return null
  const others = cards.filter((x) => x.column === column && x.id !== card.id)
  const position = before ? others.findIndex((x) => x.id === before.id) : others.length
  const now = cards.filter((x) => x.column === column).findIndex((x) => x.id === card.id)
  if (card.column === column && position === now) return null
  return position
}
