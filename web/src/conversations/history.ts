// What the chat shows of a conversation's history: the events stream's window (its newest
// entries) joined to the pages before it that the reader loaded (docs/api.md, "entries"), and
// to entries that left the window while the page was open. A window that no longer joins what
// was kept (after a reconnect that missed entries) starts the history again from itself.

import { useCallback, useEffect, useMemo, useState } from 'react'
import { errorText } from '../api'
import type { ConversationView } from './transcript'

export type Entry = NonNullable<ConversationView['entries']>[number]

// Entries in ID order, and the ID of the entry just before them, absent when they start the
// conversation: a view's window, or a page of the entries route.
export type History = { entries: Entry[]; before?: number }

// Joins the view's window to what was kept: kept entries before the window stay, the window's
// own are the view's.
export function mergeView(kept: History | null, view: ConversationView): History {
  const window = { entries: view.entries ?? [], before: view.before }
  if (!kept) return window
  const last = kept.entries.at(-1)?.id
  // It joins when it starts the conversation, or the entry just before it is one kept.
  if (view.before === undefined || last === undefined || view.before > last) return window
  const first = window.entries[0]?.id
  const older = first === undefined ? kept.entries : kept.entries.filter((e) => e.id < first)
  if (older.length === 0) return window
  return { entries: [...older, ...window.entries], before: kept.before }
}

// Adds a page loaded for the entries before `before`, if the history still starts there.
export function prependPage(kept: History, page: History, before: number): History {
  if (kept.entries[0]?.id !== before) return kept
  return { entries: [...page.entries.filter((e) => e.id < before), ...kept.entries], before: page.before }
}

// The same history: the same entries (by their IDs at both ends, and how many) and start.
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
  // What was kept: earlier pages loaded, and entries the view's window has moved past.
  const [kept, setKept] = useState<History | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // What is shown: the kept history joined to the view's window. Derived without side effects,
  // so React may compute it as often as it likes, or throw a render away.
  // biome-ignore lint/correctness/useExhaustiveDependencies: only the window matters, not each streamed word
  const history = useMemo(() => (view ? mergeView(kept, view) : null), [kept, view?.entries, view?.before])

  // Kept once committed, for when the window moves on; unchanged history isn't stored again.
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
