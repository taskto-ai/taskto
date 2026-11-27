import { useEffect, useState } from 'react'
import type { Card } from '../api'

export function localToday(now: Date = new Date()): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`
}

export function isOverdue(card: Pick<Card, 'due' | 'column'>, today: string): boolean {
  return card.due !== null && card.due < today && card.column !== 'done' && card.column !== 'archived'
}

function nextDay(ymd: string): string {
  const [y, m, d] = ymd.split('-').map(Number)
  return localToday(new Date(y, m - 1, d + 1))
}

export type DueState = 'overdue' | 'soon' | 'later'

export function dueState(card: Pick<Card, 'due' | 'column'>, today: string): DueState | null {
  if (card.due === null) return null
  if (isOverdue(card, today)) return 'overdue'
  const finished = card.column === 'done' || card.column === 'archived'
  return !finished && card.due <= nextDay(today) ? 'soon' : 'later'
}

export function useToday(): string {
  const [today, setToday] = useState(() => localToday())
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout>
    const refresh = () => {
      clearTimeout(timer)
      setToday(localToday())
      const now = new Date()
      const midnight = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1)
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
