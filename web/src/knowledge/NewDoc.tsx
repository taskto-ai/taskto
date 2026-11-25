// A new document: a name, made Markdown, created with a heading and opened to edit.

import { type FormEvent, useState } from 'react'
import { api, errorText } from '../api'
import { useT } from '../i18n'
import { type Change, encodePath, isConflict, isMarkdown, useMounted } from './docs'

export function NewDoc({
  base,
  onCreated,
  onChanged,
  onCancel,
}: {
  base: string
  // The document is made: it opens, if the form is still there.
  onCreated: (path: string) => void
  // The space's list of documents changed, whatever became of the form.
  onChanged: () => void
  onCancel: () => void
}) {
  const mounted = useMounted()
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const t = useT()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    const trimmed = name.trim().replace(/^\/+/, '')
    if (!trimmed) return
    // A new document is Markdown; other files are uploaded.
    const path = isMarkdown(trimmed) ? trimmed : `${trimmed}.md`
    const title = (path.split('/').pop() ?? path).replace(/\.md$/i, '')
    setBusy(true)
    setError(null)
    try {
      await api<Change>(`${base}/docs/${encodePath(path)}`, { method: 'PUT', body: { content: `# ${title}\n` } })
      onChanged()
      if (mounted.current) onCreated(path)
    } catch (err) {
      setError(isConflict(err) ? t('knowledge.newDoc.exists', { path }) : errorText(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form className="tt-panel tt-form" onSubmit={submit} aria-label={t('knowledge.newDoc.label')}>
      <label>
        {t('knowledge.newDoc.name')}
        <input
          value={name}
          placeholder={t('knowledge.newDoc.placeholder')}
          onChange={(e) => setName(e.target.value)}
          // biome-ignore lint/a11y/noAutofocus: the form was opened to type this
          autoFocus
        />
      </label>
      <span className="tt-hint">{t('knowledge.newDoc.hint')}</span>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      <div className="tt-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || !name.trim()}>
          {t('knowledge.newDoc.create')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}
