import { useEffect, useState } from 'react'
import { ApiError, api, type Card, type CardWithThread, type Comment, errorText } from '../api'
import { useLoad } from '../load'
import { withComments } from './comments'
import { usePoll } from './poll'

export type Send = (base: Card) => Promise<Card>
export type Fields = Record<string, unknown> | ((base: Card) => Record<string, unknown>)

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
      setLocal((latest) => {
        const base = latest ?? first
        if (!base) return latest
        const comments = withComments(base.comments, read.comments)
        return comments === base.comments ? latest : { ...base, comments }
      }),
    [url, first],
  )

  useEffect(() => {
    if (!moved || editing) return
    setLocal((latest) => {
      const base = latest ?? first
      return base && moved.version > base.version ? { ...moved, comments: base.comments } : latest
    })
  }, [moved, editing, first])

  const change = async (send: Send, base: Card | undefined = card, keepEditor = false): Promise<boolean> => {
    if (!card || !base) return false
    setBusy(true)
    setError(null)
    try {
      const updated = await send(base)
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
