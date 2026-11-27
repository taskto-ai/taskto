import type { Card } from '../api'
import { isOverdue } from './due'

export function boardFilter(cards: Card[], label: string, overdueOnly: boolean, today: string) {
  const allLabels = [...new Set([...cards.flatMap((c) => c.labels), ...(label ? [label] : [])])].sort()
  const filtering = label !== '' || overdueOnly
  const shown = (c: Card) => (!label || c.labels.includes(label)) && (!overdueOnly || isOverdue(c, today))
  const filterable = allLabels.length > 0 || cards.some((c) => c.due !== null) || filtering
  return { allLabels, filtering, shown, filterable }
}
