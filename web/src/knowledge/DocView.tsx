// One open document: its heading and actions, and what the chosen action shows (the reader,
// the editor, history, moving or deleting it). On a phone its back button returns to the list.

import { useContext, useState } from 'react'
import type { Doc } from '../api'
import { useT } from '../i18n'
import { Icon } from '../shell/Icon'
import { Content } from './Content'
import { ConfirmDelete } from './Delete'
import { encodePath, resolveDocLink, useDoc, useLatest, useMounted } from './docs'
import { Editor } from './Editor'
import { GuardContext } from './guard'
import { History } from './History'
import { Move } from './Move'

type Mode = 'view' | 'edit' | 'history' | 'move' | 'delete'

export function DocView({
  base,
  path,
  canWrite,
  admin,
  editing,
  onBack,
  onOpen,
  onChanged,
  onDeleted,
}: {
  base: string
  path: string
  canWrite: boolean
  admin: boolean
  editing: boolean
  onBack: () => void
  onOpen: (path: string) => void
  onChanged: () => void
  onDeleted: (doc: Doc) => void
}) {
  const url = `${base}/docs/${encodePath(path)}`
  const doc = useDoc(url)
  const [mode, setMode] = useState<Mode>(editing ? 'edit' : 'view')
  const text = doc.data?.content
  const t = useT()
  const onLink = (href: string) => {
    const target = resolveDocLink(path, href)
    return target === null ? null : () => onOpen(target)
  }
  const { ask } = useContext(GuardContext)
  const shown = useLatest(mode)
  const mounted = useMounted()
  const changed = () => {
    doc.reload()
    onChanged()
    setMode('view')
  }
  // A restore that finishes after the person went on to Edit (or Move) changes the document,
  // but doesn't take the screen from them: the draft they are writing stays.
  const restored = () => {
    doc.reload()
    onChanged()
    if (shown.current === 'history') setMode('view')
  }

  return (
    <article className="tt-doc" aria-label={path}>
      <div className="tt-pane-head">
        <button
          type="button"
          className="tt-icon-button tt-back"
          aria-label={t('knowledge.doc.back')}
          onClick={() => ask(onBack)}
        >
          <Icon name="back" />
        </button>
        <h2>{path}</h2>
        {doc.data && mode !== 'edit' && (
          <div className="tt-actions">
            {mode !== 'view' && (
              <button type="button" className="tt-button" onClick={() => setMode('view')}>
                {t(text !== undefined ? 'knowledge.doc.document' : 'knowledge.doc.file')}
              </button>
            )}
            {canWrite && text !== undefined && (
              <button type="button" className="tt-button tt-primary" onClick={() => setMode('edit')}>
                {t('knowledge.doc.edit')}
              </button>
            )}
            {mode !== 'history' && (
              <button type="button" className="tt-button" onClick={() => setMode('history')}>
                {t('knowledge.doc.history')}
              </button>
            )}
            {canWrite && mode !== 'move' && (
              <button type="button" className="tt-button" onClick={() => setMode('move')}>
                {t('knowledge.doc.move')}
              </button>
            )}
            {canWrite && mode !== 'delete' && (
              <button type="button" className="tt-button" onClick={() => setMode('delete')}>
                {t('knowledge.doc.delete')}
              </button>
            )}
          </div>
        )}
      </div>
      {doc.error && (
        <p role="alert" className="tt-error">
          {doc.error}
        </p>
      )}
      {!doc.data && !doc.error && <p className="tt-muted">{t('app.loading')}</p>}
      {doc.data && mode === 'edit' && text !== undefined && (
        <Editor url={url} doc={doc.data} onSaved={changed} onCancel={() => setMode('view')} />
      )}
      {doc.data && mode === 'move' && (
        <Move
          base={base}
          doc={doc.data}
          onMoved={(to) => {
            onChanged()
            // The person may have gone elsewhere, or on to edit, while it was moving: the moved
            // document opens for someone who is still here, and asks before it replaces a draft.
            if (mounted.current) ask(() => onOpen(to), 'moved')
          }}
          onReload={doc.reload}
          onCancel={() => setMode('view')}
        />
      )}
      {doc.data && mode === 'delete' && (
        <ConfirmDelete
          url={url}
          doc={doc.data}
          onDeleted={() => doc.data && onDeleted(doc.data)}
          onReload={doc.reload}
          onCancel={() => setMode('view')}
        />
      )}
      {doc.data && mode === 'history' && (
        <History
          base={base}
          url={url}
          current={doc.data}
          canWrite={canWrite}
          admin={admin}
          onLink={onLink}
          onRestored={restored}
          onReload={doc.reload}
        />
      )}
      {doc.data && (mode === 'view' || mode === 'move' || mode === 'delete') && (
        <Content url={url} doc={doc.data} onLink={onLink} />
      )}
    </article>
  )
}
