import { type FormEvent, useId, useState } from 'react'
import { api, errorText } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Dialog } from '../ui/Dialog'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { type Change, encodePath, isConflict, isMarkdown, useMounted } from './docs'

export function NewDoc({
  base,
  onCreated,
  onChanged,
  onCancel,
}: {
  base: string
  onCreated: (path: string) => void
  onChanged: () => void
  onCancel: () => void
}) {
  const mounted = useMounted()
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const t = useT()
  const form = useId()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    const trimmed = name.trim().replace(/^\/+/, '')
    if (!trimmed) return
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
    <Dialog
      open
      title={t('knowledge.newDoc.label')}
      onClose={onCancel}
      actions={
        <>
          <Button onClick={onCancel}>{t('common.cancel')}</Button>
          <Button type="submit" form={form} variant="primary" loading={busy} disabled={busy || !name.trim()}>
            {t('knowledge.newDoc.create')}
          </Button>
        </>
      }
    >
      <form id={form} className="kb-form" onSubmit={submit} aria-label={t('knowledge.newDoc.label')}>
        <Field label={t('knowledge.newDoc.name')} hint={t('knowledge.newDoc.hint')} error={error ?? undefined}>
          <Input
            data-autofocus
            value={name}
            placeholder={t('knowledge.newDoc.placeholder')}
            onChange={(e) => setName(e.target.value)}
          />
        </Field>
      </form>
    </Dialog>
  )
}
