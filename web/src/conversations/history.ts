import { useCallback, useEffect, useMemo, useState } from 'react'
import { errorText } from '../api'
import type { ConversationView } from './transcript'

export type Entry = NonNullable<ConversationView['entries']>[number]

export type History = { entries: Entry[]; before?: number }

export function mergeView(kept: History | null, view: Pick<ConversationView, 'entries' | 'before'>): History {
  const window = { entries: view.entries ?? [], before: view.before }
  if (!kept) return window
  const last = kept.entries.at(-1)?.id
  if (view.before === undefined || last === undefined || view.before > last) return window
  const first = window.entries[0]?.id
  const older = first === undefined ? kept.entries : kept.entries.filter((e) => e.id < first)
  if (older.length === 0) return window
  return { entries: [...older, ...window.entries], before: kept.before }
}

export function prependPage(kept: History, page: History, before: number): History {
  if (kept.entries[0]?.id !== before) return kept
  return { entries: [...page.entries.filter((e) => e.id < before), ...kept.entries], before: page.before }
}

function same(a: History | null, b: History): boolean {
  return (
    a !== null &&
    a.before === b.before &&
    a.entries.length === b.entries.length &&
    a.entries[0]?.id === b.entries[0]?.id &&
    a.entries.at(-1)?.id === b.entries.at(-1)?.id
  )
}

export function useHistory(view: ConversationView | null, load: (before: number) => Promise<History>) {
  const [kept, setKept] = useState<History | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const hasView = view !== null
  const entries = view?.entries
  const before = view?.before
  const history = useMemo(
    () => (hasView ? mergeView(kept, { entries, before }) : null),
    [kept, hasView, entries, before],
  )

  useEffect(() => {
    if (history) setKept((k) => (same(k, history) ? k : history))
  }, [history])

  const loadEarlier = useCallback(async () => {
    const before = history?.entries[0]?.id
    if (!history || before === undefined || history.before === undefined) return
    setLoading(true)
    setError(null)
    try {
      const page = await load(before)
      setKept((k) => prependPage(k ?? history, page, before))
    } catch (e) {
      setError(errorText(e))
    } finally {
      setLoading(false)
    }
  }, [load, history])

  return { history, loadEarlier, loading, error }
}
