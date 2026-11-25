// Uploading a file into the space, in a dialog: any file, at any path, created unless the person
// chooses to replace one already there. While it is sent, the dialog says what and how big.

import { useId, useState } from 'react'
import { ApiError, api, type Doc, errorText } from '../api'
import { useFormat, useT } from '../i18n'
import { Button } from '../ui/Button'
import { Dialog } from '../ui/Dialog'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Spinner } from '../ui/Spinner'
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
  const form = useId()

  // Without `replace`, it only creates: a file already there is the user's to replace.
  const send = async (replace: boolean) => {
    if (!file || !target) return
    setBusy(true)
    setError(null)
    try {
      const url = `${base}/docs/${encodePath(target)}`
      const text = isMarkdown(target)
        ? await markdownText(file, t('knowledge.uploadForm.notUtf8', { name: file.name }))
        : null
      // Gone since it was found: replacing it is creating it.
      const current = replace
        ? await api<Doc>(url).catch((e: unknown) => {
            if (e instanceof ApiError && e.status === 404) return null
            throw e
          })
        : null
      if (text !== null) {
        const body = { content: text }
        await api<Change>(url, { method: 'PUT', body: current ? { ...body, base_version: current.version } : body })
      } else {
        const query = current ? `?base_version=${current.version}` : ''
        await api<Change>(`${url}${query}`, { method: 'PUT', bytes: file })
      }
      onDone(target)
    } catch (err) {
      setExists(isConflict(err) && !replace)
      setError(
        isConflict(err)
          ? replace
            ? t('knowledge.uploadForm.changed', { path: target })
            : t('knowledge.uploadForm.exists', { path: target })
          : errorText(err),
      )
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open
      title={t('knowledge.uploadForm.label')}
      onClose={onCancel}
      actions={
        <>
          <Button onClick={onCancel}>{t('common.cancel')}</Button>
          {exists ? (
            <Button variant="danger" loading={busy} onClick={() => send(true)}>
              {t('knowledge.uploadForm.replace')}
            </Button>
          ) : (
            <Button type="submit" form={form} variant="primary" loading={busy} disabled={busy || !file || !target}>
              {t('knowledge.uploadForm.submit')}
            </Button>
          )}
        </>
      }
    >
      <form
        id={form}
        className="kb-form"
        aria-label={t('knowledge.uploadForm.label')}
        onSubmit={(e) => {
          e.preventDefault()
          send(false)
        }}
      >
        <Field label={t('knowledge.uploadForm.file')}>
          <Input
            data-autofocus
            type="file"
            className="kb-file-input"
            onChange={(e) => {
              const f = e.target.files?.[0] ?? null
              setFile(f)
              setExists(false)
              setError(null)
              if (f) setPath(f.name)
            }}
          />
        </Field>
        <Field label={t('knowledge.uploadForm.saveAs')} hint={t('knowledge.uploadForm.hint')}>
          <Input
            value={path}
            onChange={(e) => {
              setPath(e.target.value)
              setExists(false)
            }}
          />
        </Field>
        {busy && file && (
          <p className="kb-sending" role="status">
            <Spinner size="sm" />
            <span>{t('knowledge.uploadForm.sending', { name: file.name, size: f.bytes(file.size) })}</span>
          </p>
        )}
        {error && <Notice tone={exists ? 'warning' : 'danger'}>{error}</Notice>}
      </form>
    </Dialog>
  )
}
