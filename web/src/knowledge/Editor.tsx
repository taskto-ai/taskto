// Editing a Markdown document: the text and its preview, what changed, and the choice a
// conflict asks for (useDraft has the saving).

import { useLayoutEffect, useRef, useState } from 'react'
import type { Doc } from '../api'
import { useT } from '../i18n'
import { Markdown } from '../markdown'
import { Button } from '../ui/Button'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { SegmentedControl } from '../ui/SegmentedControl'
import { Textarea } from '../ui/Textarea'
import { useDraft } from './useDraft'
import './prose.css'

export function Editor({
  url,
  doc,
  onSaved,
  onCancel,
}: {
  url: string
  doc: Doc
  onSaved: () => void
  onCancel: () => void
}) {
  const { text, setText, message, setMessage, busy, error, conflict, save, takeTheirs, overwrite } = useDraft(
    url,
    doc,
    onSaved,
  )
  const [tab, setTab] = useState<'write' | 'preview'>('write')
  const t = useT()
  // The editor was opened to write: the keyboard goes to the text.
  const area = useRef<HTMLTextAreaElement>(null)
  useLayoutEffect(() => {
    area.current?.focus()
  }, [])

  return (
    <form
      className="kb-editor"
      aria-label={t('knowledge.editor.label')}
      onSubmit={(e) => {
        e.preventDefault()
        save()
      }}
    >
      <SegmentedControl
        label={t('knowledge.editor.view')}
        size="sm"
        value={tab}
        onChange={setTab}
        options={[
          { value: 'write', label: t('knowledge.editor.write') },
          { value: 'preview', label: t('knowledge.editor.preview') },
        ]}
      />
      {tab === 'write' ? (
        <Textarea
          ref={area}
          className="kb-editor-text"
          aria-label={t('knowledge.editor.text')}
          value={text}
          rows={18}
          onChange={(e) => setText(e.target.value)}
        />
      ) : (
        <div className="kb-editor-preview">
          <Markdown text={text} look="document" />
        </div>
      )}
      <Field label={t('knowledge.editor.message')}>
        <Input value={message} maxLength={200} onChange={(e) => setMessage(e.target.value)} />
      </Field>
      {conflict && (
        <Notice
          tone="danger"
          action={
            <span className="kb-actions">
              {conflict.current && (
                <Button size="sm" onClick={() => conflict.current && takeTheirs(conflict.current)}>
                  {t('knowledge.editor.loadTheirs')}
                </Button>
              )}
              <Button size="sm" variant="danger" disabled={busy} onClick={() => overwrite(conflict.current)}>
                {t(conflict.current ? 'knowledge.editor.overwrite' : 'knowledge.editor.saveAgain')}
              </Button>
            </span>
          }
        >
          {t(conflict.current ? 'knowledge.editor.changed' : 'knowledge.editor.gone')}
        </Notice>
      )}
      {error && <Notice tone="danger">{error}</Notice>}
      <div className="kb-actions">
        <Button type="submit" variant="primary" loading={busy} disabled={busy || conflict !== null}>
          {t('knowledge.editor.save')}
        </Button>
        <Button onClick={onCancel}>{t('common.cancel')}</Button>
      </div>
    </form>
  )
}
