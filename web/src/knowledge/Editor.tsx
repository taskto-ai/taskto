// Editing a Markdown document: the text and its preview, what changed, and the choice a
// conflict asks for (useDraft has the saving).

import { useContext, useEffect, useRef, useState } from 'react'
import type { Doc } from '../api'
import { useT } from '../i18n'
import { Markdown } from '../markdown'
import { GuardContext } from './guard'
import { useDraft } from './useDraft'

// What the question says, by why it is asked.
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
  // Whatever would close the editor asks first while the draft differs from what was loaded.
  const guard = useContext(GuardContext)
  const { report, saved, dispose, asking, keep } = guard
  const { text, setText, message, setMessage, dirty, busy, error, conflict, save, takeTheirs, overwrite } = useDraft(
    url,
    doc,
    () => {
      onSaved()
      saved()
    },
  )
  const [tab, setTab] = useState<'write' | 'preview'>('write')
  const t = useT()
  useEffect(() => {
    report(dirty, busy)
  }, [dirty, busy, report])
  useEffect(() => dispose, [dispose])

  // The question comes into view and takes the focus; "Keep editing", by button or Escape,
  // gives the focus back to the text, with the cursor where it was.
  const question = useRef<HTMLFieldSetElement>(null)
  const keepButton = useRef<HTMLButtonElement>(null)
  const textarea = useRef<HTMLTextAreaElement>(null)
  const previewTab = useRef<HTMLButtonElement>(null)
  const cursor = useRef<[number, number]>([0, 0])
  useEffect(() => {
    if (!asking) return
    const ta = textarea.current
    if (ta) cursor.current = [ta.selectionStart, ta.selectionEnd]
    question.current?.scrollIntoView?.({ block: 'nearest' })
    keepButton.current?.focus()
  }, [asking])
  const keepEditing = () => {
    keep()
    const ta = textarea.current
    if (ta) {
      ta.focus()
      ta.setSelectionRange(cursor.current[0], cursor.current[1])
    } else previewTab.current?.focus()
  }
  useEffect(() => {
    if (!asking) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.preventDefault()
      keepEditing()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  })

  return (
    <form
      className="tt-editor"
      aria-label={t('knowledge.editor.label')}
      onSubmit={(e) => {
        e.preventDefault()
        save()
      }}
    >
      <fieldset className="tt-view-tabs" aria-label={t('knowledge.editor.view')}>
        <button type="button" aria-pressed={tab === 'write'} onClick={() => setTab('write')}>
          {t('knowledge.editor.write')}
        </button>
        <button ref={previewTab} type="button" aria-pressed={tab === 'preview'} onClick={() => setTab('preview')}>
          {t('knowledge.editor.preview')}
        </button>
      </fieldset>
      {tab === 'write' ? (
        <textarea
          ref={textarea}
          aria-label={t('knowledge.editor.text')}
          value={text}
          rows={18}
          onChange={(e) => setText(e.target.value)}
        />
      ) : (
        <div className="tt-preview">
          <Markdown text={text} />
        </div>
      )}
      <input
        aria-label={t('knowledge.editor.message')}
        placeholder={t('knowledge.editor.message')}
        value={message}
        maxLength={200}
        onChange={(e) => setMessage(e.target.value)}
      />
      {conflict && (
        <div role="alert" className="tt-conflict">
          <span>{t(conflict.current ? 'knowledge.editor.changed' : 'knowledge.editor.gone')}</span>
          {conflict.current && (
            <button
              type="button"
              className="tt-button"
              onClick={() => conflict.current && takeTheirs(conflict.current)}
            >
              {t('knowledge.editor.loadTheirs')}
            </button>
          )}
          <button
            type="button"
            className="tt-button tt-danger"
            disabled={busy}
            onClick={() => overwrite(conflict.current)}
          >
            {t(conflict.current ? 'knowledge.editor.overwrite' : 'knowledge.editor.saveAgain')}
          </button>
        </div>
      )}
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      {asking && (
        // The same question-and-two-buttons panel as deleting a document.
        <fieldset
          ref={question}
          className="tt-panel"
          role="alertdialog"
          aria-label={t('knowledge.editor.discardLabel')}
        >
          <p>{t(DISCARD_QUESTION[asking])}</p>
          <div className="tt-actions">
            <button type="button" className="tt-button tt-danger" onClick={guard.discard}>
              {t('knowledge.editor.discard')}
            </button>
            <button ref={keepButton} type="button" className="tt-button" onClick={keepEditing}>
              {t('knowledge.editor.keepEditing')}
            </button>
          </div>
        </fieldset>
      )}
      <div className="tt-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || conflict !== null}>
          {t('knowledge.editor.save')}
        </button>
        <button type="button" className="tt-button" onClick={() => guard.ask(onCancel)}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}
