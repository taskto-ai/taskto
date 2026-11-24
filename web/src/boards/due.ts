// A card's due date is a calendar date (YYYY-MM-DD) with no time zone, so "today" is the
// reader's own day: the browser's local date, never the server's.

import type { Card } from '../api'

// Today's date where the reader is, as YYYY-MM-DD.
export function localToday(now: Date = new Date()): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`
}

// Overdue: due before today, and not finished (a done or archived card isn't late).
export function isOverdue(card: Pick<Card, 'due' | 'column'>, today: string): boolean {
  return card.due !== null && card.due < today && card.column !== 'done' && card.column !== 'archived'
}
