// One open document: a bar with where it is (space, folders, name), its size and its actions
// (Edit, History, and a menu with Move and Delete), and what the chosen action shows (the
// reader, the editor, history). Moving and deleting ask in a dialog. On a phone, or when it was
// opened from search results, its back button returns there.

import { useContext, useLayoutEffect, useRef, useState } from 'react'
import type { Doc } from '../api'
import { useFormat, useT } from '../i18n'
import { Button } from '../ui/Button'
import { Heading } from '../ui/Heading'
import { Icon } from '../ui/Icon'
import { IconButton } from '../ui/IconButton'
import { MenuItem, MenuSeparator, MoreMenu } from '../ui/Menu'
import { Notice } from '../ui/Notice'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { Content } from './Content'
import { ConfirmDelete } from './Delete'
import { encodePath, resolveDocLink, useDoc, useLatest, useMounted } from './docs'
import { Editor } from './Editor'
import { focusLost } from './focus'
import { GuardContext } from './guard'
import { History } from './History'
import { Move } from './Move'

type Mode = 'view' | 'edit' | 'history'

export function DocView({
  base,
  path,
  space,
  canWrite,
  admin,
  editing,
  back,
  takeFocus,
  onBack,
  onOpen,
  onChanged,
  onDeleted,
  onEditing,
}: {
  base: string
  path: string
  // The space's name, for the path line.
  space: string
  canWrite: boolean
  admin: boolean
  editing: boolean
  // Where the back button returns: the list (on a phone) or the search results (always shown).
  back: 'list' | 'results'
  // Opened by something other than the tree (a link, a search result, a new document): the
  // keyboard moves to the document.
  takeFocus: boolean
  onBack: () => void
  onOpen: (path: string) => void
  onChanged: () => void
  onDeleted: (doc: Doc) => void
  // The editor opened or closed: its Save is then the page's primary action.
  onEditing?: (editing: boolean) => void
}) {
  const url = `${base}/docs/${encodePath(path)}`
  const doc = useDoc(url)
  const [mode, setMode] = useState<Mode>(editing ? 'edit' : 'view')
  const [asking, setAsking] = useState<'move' | 'delete' | null>(null)
  const text = doc.data?.content
  const t = useT()
  const f = useFormat()
  const isEditing = mode === 'edit' && text !== undefined
  const reportEditing = useRef(onEditing)
  reportEditing.current = onEditing
  // Before the screen is painted, so New and Save are never both primary.
  useLayoutEffect(() => {
    reportEditing.current?.(isEditing)
    return () => reportEditing.current?.(false)
  }, [isEditing])
  const onLink = (href: string) => {
    const target = resolveDocLink(path, href)
    return target === null ? null : () => onOpen(target)
  }

  // Where the keyboard goes when the view changes: the heading when the document opens (from
  // anywhere but the tree), Edit after saving or cancelling an edit, the heading after a
  // restore, and otherwise the button that brings back what was there when the focus was lost.
  const article = useRef<HTMLElement>(null)
  const editButton = useRef<HTMLButtonElement>(null)
  const toggleButton = useRef<HTMLButtonElement>(null)
  const next = useRef<'edit' | 'heading' | null>(null)
  const first = useRef(true)
  const lastMode = useRef(mode)
  const heading = () => article.current?.querySelector<HTMLElement>('h2')
  // Also when the document comes (back): Edit is drawn only then.
  // biome-ignore lint/correctness/useExhaustiveDependencies: runs when the view changes or the document comes
  useLayoutEffect(() => {
    const modeChanged = lastMode.current !== mode
    lastMode.current = mode
    if (first.current) {
      first.current = false
      // A page that just loaded has the keyboard nowhere yet: that is not a focus to restore.
      const hidden = document.activeElement !== document.body && focusLost()
      if (mode !== 'edit' && (takeFocus || hidden)) heading()?.focus()
      return
    }
    const want = next.current
    if (want === 'edit') {
      // Kept until Edit is there again (the document reloads after a save).
      if (!editButton.current) return
      next.current = null
      return editButton.current.focus()
    }
    next.current = null
    if (want === 'heading') return heading()?.focus()
    if (modeChanged && focusLost()) (toggleButton.current ?? heading())?.focus()
  }, [mode, doc.data !== undefined])

  const { ask } = useContext(GuardContext)
  const shown = useLatest(mode)
  const mounted = useMounted()
  const changed = (focus: 'edit' | 'heading') => {
    next.current = focus
    doc.reload()
    onChanged()
    setMode('view')
  }
  // A restore that finishes after the person went on to Edit (or Move) changes the document,
  // but doesn't take the screen from them: the draft they are writing stays.
  const restored = () => {
    doc.reload()
    onChanged()
    if (shown.current === 'history') {
      next.current = 'heading'
      setMode('view')
    }
  }

  const name = path.split('/').pop() ?? path
  const folders = path.split('/').slice(0, -1)

  return (
    <article className="kb-doc" aria-label={path} ref={article}>
      <div className="kb-doc-bar">
        <span className="kb-back" data-back={back}>
          <IconButton
            icon="back"
            label={t(back === 'results' ? 'knowledge.doc.backToResults' : 'knowledge.doc.back')}
            // Back to the results keeps the document behind them, draft and all; back to the list
            // closes it, so a draft that would be lost asks first.
            onClick={() => (back === 'results' ? onBack() : ask(onBack))}
          />
        </span>
        <div className="kb-doc-where">
          <Heading level={2} size="subsection" className="kb-doc-path" tabIndex={-1}>
            {space && (
              <>
                <span className="kb-doc-crumb">{space}</span>
                <Separator />
              </>
            )}
            {folders.map((folder, i) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: a path's folders have no identity but their place
              <span key={i} className="kb-doc-crumbs">
                <span className="kb-doc-crumb">{folder}</span>
                <Separator />
              </span>
            ))}
            <span className="kb-doc-name">{name}</span>
          </Heading>
          {doc.data && (
            <span className="kb-doc-meta">
              {doc.data.versioned === false
                ? t('knowledge.sizeUnversioned', { size: f.bytes(doc.data.size) })
                : f.bytes(doc.data.size)}
            </span>
          )}
        </div>
        {doc.data && mode !== 'edit' && (
          <div className="kb-doc-actions">
            {mode === 'history' ? (
              <Button ref={toggleButton} variant="ghost" icon="back" onClick={() => setMode('view')}>
                {t(text !== undefined ? 'knowledge.doc.document' : 'knowledge.doc.file')}
              </Button>
            ) : (
              <>
                {canWrite && text !== undefined && (
                  <Button ref={editButton} icon="edit" onClick={() => setMode('edit')}>
                    {t('knowledge.doc.edit')}
                  </Button>
                )}
                <Button ref={toggleButton} variant="ghost" icon="clock" onClick={() => setMode('history')}>
                  {t('knowledge.doc.history')}
                </Button>
              </>
            )}
            {canWrite && (
              <MoreMenu>
                <MenuItem icon="move" onSelect={() => setAsking('move')}>
                  {t('knowledge.doc.move')}
                </MenuItem>
                <MenuSeparator />
                <MenuItem tone="danger" onSelect={() => setAsking('delete')}>
                  {t('knowledge.doc.delete')}
                </MenuItem>
              </MoreMenu>
            )}
          </div>
        )}
      </div>
      {doc.error && (
        <Notice
          tone="danger"
          title={t('knowledge.errors.document')}
          action={
            <Button size="sm" onClick={doc.reload}>
              {t('app.tryAgain')}
            </Button>
          }
        >
          {doc.error}
        </Notice>
      )}
      {!doc.data && !doc.error && <DocSkeleton />}
      {doc.data && mode === 'edit' && text !== undefined && (
        <Editor
          url={url}
          doc={doc.data}
          onSaved={() => changed('edit')}
          onCancel={() => {
            next.current = 'edit'
            setMode('view')
          }}
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
      {doc.data && mode === 'view' && <Content url={url} doc={doc.data} onLink={onLink} />}
      {doc.data && asking === 'move' && (
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
          onCancel={() => setAsking(null)}
        />
      )}
      {doc.data && asking === 'delete' && (
        <ConfirmDelete
          url={url}
          doc={doc.data}
          onDeleted={() => doc.data && onDeleted(doc.data)}
          onReload={doc.reload}
          onCancel={() => setAsking(null)}
        />
      )}
    </article>
  )
}

// Between the parts of the path: a chevron to see, a slash to hear.
function Separator() {
  const t = useT()
  return (
    <>
      <Icon name="chevron-end" size={14} className="kb-doc-separator" />
      <VisuallyHidden>{t('knowledge.doc.separator')}</VisuallyHidden>
    </>
  )
}

// The shape of a document while it loads: a title and some lines.
function DocSkeleton() {
  const t = useT()
  return (
    <div className="kb-doc-loading" aria-busy="true">
      <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
      <Skeleton width="55%" height="var(--space-8)" />
      {['100%', '92%', '96%', '70%'].map((width, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: placeholders have no identity but their place
        <Skeleton key={i} width={width} />
      ))}
      <Skeleton shape="block" />
    </div>
  )
}
