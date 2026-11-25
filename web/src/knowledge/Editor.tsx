// Editing a Markdown document: the text and its preview, what changed, and the choice a
// conflict asks for (useDraft has the saving).

import { useContext, useEffect, useState } from 'react'
import type { Doc } from '../api'
import { useT } from '../i18n'
import { Markdown } from '../markdown'
import { GuardContext } from './guard'
import { useDraft } from './useDraft'

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
  const { text, setText, message, setMessage, dirty, busy, error, conflict, save, takeTheirs, overwrite } = useDraft(
    url,
    doc,
    onSaved,
  )
  const [tab, setTab] = useState<'write' | 'preview'>('write')
  const t = useT()
  // Whatever would close the editor asks first while the draft differs from what was loaded.
  const guard = useContext(GuardContext)
  const { report } = guard
  useEffect(() => {
    report(dirty)
    return () => report(false)
  }, [dirty, report])

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
        <button type="button" aria-pressed={tab === 'preview'} onClick={() => setTab('preview')}>
          {t('knowledge.editor.preview')}
        </button>
      </fieldset>
      {tab === 'write' ? (
        <textarea
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
      {guard.asking && (
        // The same question-and-two-buttons panel as deleting a document.
        <fieldset className="tt-panel" aria-label={t('knowledge.editor.discardLabel')}>
          <p>{t('knowledge.editor.discardQuestion')}</p>
          <div className="tt-actions">
            <button type="button" className="tt-button tt-danger" onClick={guard.discard}>
              {t('knowledge.editor.discard')}
            </button>
            <button type="button" className="tt-button" onClick={guard.keep}>
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
