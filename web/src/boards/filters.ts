import type { Card } from '../api'
import { isOverdue } from './due'

// The board's filters, applied to the cards already loaded: a label ('' for any), and only the
// overdue. The chosen label stays chosen, and listed, when no card has it any more (a reload took
// its last card): the board shows no cards until the person clears the filter.
export function boardFilter(cards: Card[], label: string, overdueOnly: boolean, today: string) {
  const allLabels = [...new Set([...cards.flatMap((c) => c.labels), ...(label ? [label] : [])])].sort()
  const filtering = label !== '' || overdueOnly
  const shown = (c: Card) => (!label || c.labels.includes(label)) && (!overdueOnly || isOverdue(c, today))
  // The filters are offered when there is something to filter by, or a filter is on.
  const filterable = allLabels.length > 0 || cards.some((c) => c.due !== null) || filtering
  return { allLabels, filtering, shown, filterable }
}
