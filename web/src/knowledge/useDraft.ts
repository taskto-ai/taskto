// An edit of a Markdown document and its saving. Saving sends the version the edit started
// from; if the document changed (or went away) meanwhile, the edit is kept and the writer
// chooses: take the current text, or save theirs over it.

import { useState } from 'react'
import { ApiError, api, type Doc, errorText } from '../api'
import { useT } from '../i18n'
import { type Change, isConflict } from './docs'

export function useDraft(url: string, doc: Doc, onSaved: () => void) {
  const [text, setText] = useState(doc.content ?? '')
  const [base, setBase] = useState<string | null>(doc.version)
  // The text the draft started from (or was last replaced by): what "unchanged" means.
  const [loaded, setLoaded] = useState(doc.content ?? '')
  const [message, setMessage] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // The document as it is now, after a 409; null if it is gone.
  const [conflict, setConflict] = useState<{ current: Doc | null } | null>(null)
  const t = useT()

  const save = async (version: string | null) => {
    setBusy(true)
    setError(null)
    try {
      await api<Change>(url, {
        method: 'PUT',
        body: {
          content: text,
          ...(version ? { base_version: version } : {}),
          ...(message.trim() ? { message: message.trim() } : {}),
        },
      })
      onSaved()
    } catch (err) {
      if (!isConflict(err)) {
        setError(errorText(err))
        return
      }
      try {
        setConflict({ current: await api<Doc>(url) })
      } catch (e) {
        if (e instanceof ApiError && e.status === 404) setConflict({ current: null })
        else setError(t('knowledge.editor.reloadFailed', { error: errorText(e) }))
      }
    } finally {
      setBusy(false)
    }
  }

  const takeTheirs = (current: Doc) => {
    setText(current.content ?? '')
    setLoaded(current.content ?? '')
    setBase(current.version)
    setConflict(null)
  }
  const overwrite = (current: Doc | null) => {
    const version = current?.version ?? null
    setBase(version)
    setConflict(null)
    save(version)
  }

  // The draft differs from what was loaded: closing it would lose something.
  const dirty = text !== loaded || message.trim() !== ''

  return {
    text,
    setText,
    message,
    setMessage,
    dirty,
    busy,
    error,
    conflict,
    save: () => save(base),
    takeTheirs,
    overwrite,
  }
}
