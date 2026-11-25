// Uploading a file into the space: any file, at any path, created unless the person chooses to
// replace one already there.

import { useEffect, useRef, useState } from 'react'
import { ApiError, api, type Doc, errorText } from '../api'
import { useFormat, useT } from '../i18n'
import { type Change, encodePath, isConflict, isMarkdown } from './docs'

// A Markdown file is sent as its text, checked here to be UTF-8; any other file as its bytes,
// streamed by the browser whatever its size.
async function markdownText(file: File, notUtf8: string): Promise<string> {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(await file.arrayBuffer())
  } catch {
    throw new Error(notUtf8)
  }
}

export function Upload({
  base,
  onDone,
  onCancel,
}: {
  base: string
  onDone: (path: string) => void
  onCancel: () => void
}) {
  const [file, setFile] = useState<File | null>(null)
  const [path, setPath] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [exists, setExists] = useState(false)
  const target = path.trim().replace(/^\/+/, '')
  const t = useT()
  const f = useFormat()

  // The upload in flight: Cancel, or the form going away, stops it, and then nothing opens.
  const sending = useRef<AbortController | null>(null)
  useEffect(() => () => sending.current?.abort(), [])
  const cancel = () => {
    sending.current?.abort()
    onCancel()
  }

  // Without `replace`, it only creates: a file already there is the user's to replace.
  const send = async (replace: boolean) => {
    if (!file || !target) return
    setBusy(true)
    setError(null)
    sending.current = new AbortController()
    const { signal } = sending.current
    try {
      const url = `${base}/docs/${encodePath(target)}`
      const text = isMarkdown(target)
        ? await markdownText(file, t('knowledge.uploadForm.notUtf8', { name: file.name }))
        : null
      // Gone since it was found: replacing it is creating it.
      const current = replace
        ? await api<Doc>(url, { signal }).catch((e: unknown) => {
            if (e instanceof ApiError && e.status === 404) return null
            throw e
          })
        : null
      if (text !== null) {
        const body = { content: text }
        await api<Change>(url, {
          method: 'PUT',
          body: current ? { ...body, base_version: current.version } : body,
          signal,
        })
      } else {
        const query = current ? `?base_version=${current.version}` : ''
        await api<Change>(`${url}${query}`, { method: 'PUT', bytes: file, signal })
      }
      if (!signal.aborted) onDone(target)
    } catch (err) {
      if (signal.aborted) return
      setExists(isConflict(err) && !replace)
      setError(
        isConflict(err)
          ? replace
            ? t('knowledge.uploadForm.changed', { path: target })
            : t('knowledge.uploadForm.exists', { path: target })
          : errorText(err),
      )
    } finally {
      if (!signal.aborted) setBusy(false)
    }
  }

  return (
    <form
      className="tt-panel tt-form"
      aria-label={t('knowledge.uploadForm.label')}
      onSubmit={(e) => {
        e.preventDefault()
        send(false)
      }}
    >
      <label>
        {t('knowledge.uploadForm.file')}
        <input
          type="file"
          onChange={(e) => {
            const f = e.target.files?.[0] ?? null
            setFile(f)
            setExists(false)
            setError(null)
            if (f) setPath(f.name)
          }}
        />
      </label>
      <label>
        {t('knowledge.uploadForm.saveAs')}
        <input
          value={path}
          onChange={(e) => {
            setPath(e.target.value)
            setExists(false)
          }}
        />
      </label>
      <span className="tt-hint">{t('knowledge.uploadForm.hint')}</span>
      {busy && file && (
        <p className="tt-muted" role="status">
          {t('knowledge.uploadForm.sending', { name: file.name, size: f.bytes(file.size) })}
        </p>
      )}
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      <div className="tt-actions">
        {exists ? (
          <button type="button" className="tt-button tt-danger" disabled={busy} onClick={() => send(true)}>
            {t('knowledge.uploadForm.replace')}
          </button>
        ) : (
          <button type="submit" className="tt-button tt-primary" disabled={busy || !file || !target}>
            {t('knowledge.uploadForm.submit')}
          </button>
        )}
        <button type="button" className="tt-button" onClick={cancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}
