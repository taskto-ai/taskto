import { useContext, useEffect, useId, useLayoutEffect, useRef, useState } from 'react'
import type { Doc } from '../api'
import { useT } from '../i18n'
import { Markdown } from '../markdown'
import { Button } from '../ui/Button'
import { ConfirmDialog } from '../ui/ConfirmDialog'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { SegmentedControl } from '../ui/SegmentedControl'
import { Textarea } from '../ui/Textarea'
import { GuardContext } from './guard'
import { useDraft } from './useDraft'
import './prose.css'

const DISCARD_QUESTION = {
  leave: 'knowledge.editor.discardQuestion',
  deleted: 'knowledge.editor.discardDeleted',
  moved: 'knowledge.editor.discardMoved',
} as const

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
  const guard = useContext(GuardContext)
  const { report, saved, dispose, asking, keep } = guard
  const me = useId()
  const { text, setText, message, setMessage, dirty, busy, error, conflict, save, takeTheirs, overwrite } = useDraft(
    url,
    doc,
    () => {
      onSaved()
      saved(me)
    },
  )
  const [tab, setTab] = useState<'write' | 'preview'>('write')
  const t = useT()
  useEffect(() => {
    report(me, dirty, busy)
  }, [me, dirty, busy, report])
  useEffect(() => () => dispose(me), [me, dispose])

  const form = useRef<HTMLFormElement>(null)
  const area = useRef<HTMLTextAreaElement>(null)
  useLayoutEffect(() => {
    area.current?.focus()
  }, [])

  const cursor = useRef<[number, number]>([0, 0])
  const backToText = useRef(false)
  useLayoutEffect(() => {
    if (asking) {
      const ta = area.current
      if (ta) cursor.current = [ta.selectionStart, ta.selectionEnd]
      return
    }
    if (!backToText.current) return
    backToText.current = false
    const ta = area.current
    if (ta) {
      ta.focus()
      ta.setSelectionRange(cursor.current[0], cursor.current[1])
    } else form.current?.querySelector<HTMLElement>('input[type="radio"]:checked')?.focus()
  }, [asking])
  const keepEditing = () => {
    backToText.current = true
    keep()
  }

  return (
    <form
      ref={form}
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
        <Button onClick={() => guard.ask(onCancel)}>{t('common.cancel')}</Button>
      </div>
      <ConfirmDialog
        open={asking !== null}
        title={t('knowledge.editor.discardLabel')}
        confirmLabel={t('knowledge.editor.discard')}
        cancelLabel={t('knowledge.editor.keepEditing')}
        tone="danger"
        onConfirm={guard.discard}
        onCancel={keepEditing}
      >
        {asking && t(DISCARD_QUESTION[asking])}
      </ConfirmDialog>
    </form>
  )
}
