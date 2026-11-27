import { useCallback, useContext, useEffectEvent, useLayoutEffect, useRef, useState } from 'react'
import type { Doc } from '../api'
import { byPlace } from '../byPlace'
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
import { useLoadingStatus } from './loading'
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
  space: string
  canWrite: boolean
  admin: boolean
  editing: boolean
  back: 'list' | 'results'
  takeFocus: boolean
  onBack: () => void
  onOpen: (path: string) => void
  onChanged: () => void
  onDeleted: (doc: Doc) => void
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
  useLayoutEffect(() => {
    reportEditing.current?.(isEditing)
    return () => reportEditing.current?.(false)
  }, [isEditing])
  const onLink = (href: string) => {
    const target = resolveDocLink(path, href)
    return target === null ? null : () => onOpen(target)
  }

  const article = useRef<HTMLElement>(null)
  const editButton = useRef<HTMLButtonElement>(null)
  const toggleButton = useRef<HTMLButtonElement>(null)
  const next = useRef<'edit' | 'heading' | null>(null)
  const first = useRef(true)
  const lastMode = useRef(mode)
  const heading = () => article.current?.querySelector<HTMLElement>('h2')
  const focusEditIfWanted = useCallback(() => {
    if (next.current !== 'edit' || !editButton.current) return
    next.current = null
    editButton.current.focus()
  }, [])
  const editButtonRef = useCallback(
    (el: HTMLButtonElement | null) => {
      editButton.current = el
      focusEditIfWanted()
    },
    [focusEditIfWanted],
  )
  const placeFocus = useEffectEvent((mode: Mode) => {
    const modeChanged = lastMode.current !== mode
    lastMode.current = mode
    if (first.current) {
      first.current = false
      const hidden = document.activeElement !== document.body && focusLost()
      if (mode !== 'edit' && (takeFocus || hidden)) heading()?.focus()
      return
    }
    if (next.current === 'edit') return focusEditIfWanted()
    const want = next.current
    next.current = null
    if (want === 'heading') return heading()?.focus()
    if (modeChanged && focusLost()) (toggleButton.current ?? heading())?.focus()
  })
  useLayoutEffect(() => placeFocus(mode), [mode])

  const { ask } = useContext(GuardContext)
  const shown = useLatest(mode)
  const mounted = useMounted()
  const changed = (focus: 'edit' | 'heading') => {
    next.current = focus
    doc.reload()
    onChanged()
    setMode('view')
  }
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
            {byPlace(folders).map(({ key, item: folder }) => (
              <span key={key} className="kb-doc-crumbs">
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
                  <Button ref={editButtonRef} icon="edit" onClick={() => setMode('edit')}>
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
            <Button
              size="sm"
              onClick={() => {
                heading()?.focus()
                doc.reload()
              }}
            >
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

function Separator() {
  const t = useT()
  return (
    <>
      <Icon name="chevron-end" size={14} className="kb-doc-separator" />
      <VisuallyHidden>{t('knowledge.doc.separator')}</VisuallyHidden>
    </>
  )
}

function DocSkeleton() {
  useLoadingStatus(true)
  return (
    <div className="kb-doc-loading">
      <Skeleton width="55%" height="var(--space-8)" />
      {['100%', '92%', '96%', '70%'].map((width) => (
        <Skeleton key={width} width={width} />
      ))}
      <Skeleton shape="block" />
    </div>
  )
}
