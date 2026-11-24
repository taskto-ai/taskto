// A card's due date is a calendar date (YYYY-MM-DD) with no time zone, so "today" is the
// reader's own day: the browser's local date, never the server's.

import { useEffect, useState } from 'react'
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

// Today's date where the reader is, kept current: it changes at the next local midnight, and
// again when the page becomes visible (a timer may not have fired while it slept).
export function useToday(): string {
  const [today, setToday] = useState(() => localToday())
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout>
    const refresh = () => {
      clearTimeout(timer)
      setToday(localToday())
      const now = new Date()
      const midnight = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1)
      // A second past it, so the clock has surely turned over.
      timer = setTimeout(refresh, midnight.getTime() - now.getTime() + 1000)
    }
    const visible = () => {
      if (document.visibilityState === 'visible') refresh()
    }
    refresh()
    document.addEventListener('visibilitychange', visible)
    return () => {
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', visible)
    }
  }, [])
  return today
}
