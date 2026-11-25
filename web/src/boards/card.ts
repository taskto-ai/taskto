import { useEffect, useState } from 'react'
import { ApiError, api, type Card, type CardWithThread, type Comment, errorText } from '../api'
import { useLoad } from '../load'
import { withComments } from './comments'
import { usePoll } from './poll'

// A change to the card, made over the card it is given: with its version, and from its lists
// for a change to one (the server replaces a list whole), so redoing it over the current card
// keeps what others added.
export type Send = (base: Card) => Promise<Card>
export type Fields = Record<string, unknown> | ((base: Card) => Record<string, unknown>)

// The open card, read and changed: each change is sent with the version shown; a 409 carries the
// card as it is now, and the writer either takes it (their change is dropped) or sends the same
// change again on top of it. The thread is read again every few seconds while the page is
// visible, for others' comments; the card's fields aren't, so a form being filled in never
// changes under the writer (a stale change still meets the 409). `moved` is the card as a drag on
// the board moved it; onChanged is told of every change, for the board to read its cards again.
export function useCard(url: string, moved: Card | null, onChanged: () => void) {
  const loaded = useLoad(() => api<CardWithThread>(url), [url])
  const [local, setLocal] = useState<CardWithThread | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [conflict, setConflict] = useState<{ current: Card; send: Send } | null>(null)
  const [editing, setEditing] = useState(false)
  const card = local ?? loaded.data
  const first = loaded.data

  const stopped = usePoll(
    first ? () => api<CardWithThread>(url) : null,
    (read) =>
      // Onto the latest card, like every change: only the thread is taken from the read.
      setLocal((latest) => {
        const base = latest ?? first
        if (!base) return latest
        const comments = withComments(base.comments, read.comments)
        return comments === base.comments ? latest : { ...base, comments }
      }),
    [url, first],
  )

  // The card was dragged to another column: show it there, unless the writer is editing it,
  // whose save then meets the 409 and chooses.
  useEffect(() => {
    if (!moved || editing) return
    setLocal((latest) => {
      const base = latest ?? first
      return base && moved.version > base.version ? { ...moved, comments: base.comments } : latest
    })
  }, [moved, editing, first])

  // keepEditor: a change from elsewhere on the card (its due date, labels) doesn't close the
  // editor of the title and description, whose unsaved draft would be lost.
  const change = async (send: Send, base: Card | undefined = card, keepEditor = false): Promise<boolean> => {
    if (!card || !base) return false
    setBusy(true)
    setError(null)
    try {
      const updated = await send(base)
      // Onto the latest card: a comment added while this was in flight stays.
      setLocal((latest) => ({ ...updated, comments: (latest ?? card).comments }))
      setConflict(null)
      if (!keepEditor) setEditing(false)
      onChanged()
      return true
    } catch (err) {
      const current = err instanceof ApiError && err.status === 409 && (err.body as { current?: Card })?.current
      if (current) setConflict({ current, send })
      else setError(errorText(err))
      return false
    } finally {
      setBusy(false)
    }
  }
  const takeTheirs = (current: Card) => {
    if (card) setLocal((latest) => ({ ...current, comments: (latest ?? card).comments }))
    setConflict(null)
    setEditing(false)
    onChanged()
  }
  // A comment added: onto the latest card, so a change saved while the comment was in flight stays.
  const addComment = (c: Comment) => {
    if (!card) return
    setLocal((latest) => {
      const base = latest ?? card
      return { ...base, comments: [...base.comments, c] }
    })
  }

  const patch = (fields: Fields) => (base: Card) =>
    api<Card>(url, {
      method: 'PATCH',
      body: { version: base.version, ...(typeof fields === 'function' ? fields(base) : fields) },
    })
  const post =
    (action: string, fields: Record<string, unknown> = {}) =>
    (base: Card) =>
      api<Card>(`${url}/${action}`, { method: 'POST', body: { version: base.version, ...fields } })

  return {
    card,
    loadError: loaded.error,
    busy,
    error,
    conflict,
    stopped,
    editing,
    setEditing,
    change,
    takeTheirs,
    addComment,
    patch,
    post,
  }
}
