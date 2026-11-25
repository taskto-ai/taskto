// The knowledge base: a space's documents beside the open one, and search. Writers create,
// edit, upload, move, delete and restore; viewers only read. Every change names the version
// it was based on, so a document someone changed meanwhile is a conflict to settle (reload,
// or overwrite on purpose), never a silent overwrite. On a phone it is the list or the
// document, with a way back.

// The page composes the parts: beside the tree of documents (DocList), with the space and the
// search box above it, the open document (DocView: the reader, Editor, History, Move,
// ConfirmDelete) or the search results; the dialogs that add a document (NewDoc, Upload), and
// the notice after a delete (Deleted). docs.ts has the data.

import { useLayoutEffect, useRef, useState } from 'react'
import { ws } from '../api'
import { useT } from '../i18n'
import { parseDocumentItem } from '../router'
import { SearchBox, type SearchQuery, SearchResults } from '../search/Search'
import { spaceLabel, useSpaces } from '../spaces'
import { Button } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { Heading } from '../ui/Heading'
import { Icon } from '../ui/Icon'
import { IconButton } from '../ui/IconButton'
import { Notice } from '../ui/Notice'
import { Page } from '../ui/Page'
import { Select } from '../ui/Select'
import { Deleted } from './Delete'
import { DocList } from './DocList'
import { DocView } from './DocView'
import { useDocs } from './docs'
import { focusLost } from './focus'
import { NewDoc } from './NewDoc'
import { Upload } from './Upload'
import './knowledge.css'

// admin: the user is an admin of the workspace, who may remove a file's old versions from
// its history.
// open is a link's item (documentItem): the space and document to open on.
export function KnowledgePage({
  workspace,
  admin = false,
  open: link,
}: {
  workspace: number
  admin?: boolean
  open?: string
}) {
  const target = parseDocumentItem(link)
  const { spaces, space, choose } = useSpaces(workspace, target?.space)
  const base = space ? `${ws(workspace)}/spaces/${space.id}` : null
  const docs = useDocs(base)
  // fromTree: opened from the tree, where the keyboard may stay; from anywhere else, the
  // document takes it. One a link opens with the page leaves the keyboard where the page puts it.
  const [open, setOpen] = useState<{ path: string; editing?: boolean; fromTree?: boolean } | null>(
    target?.path ? { path: target.path, fromTree: true } : null,
  )
  // Each search counts, so asking the same again searches again rather than showing the last.
  const [query, setQuery] = useState<(SearchQuery & { n: number }) | null>(null)
  // The results are in front of the open document: a search was made, or Back to the results.
  // The document stays drawn behind them (hidden), so an edit under way is kept.
  const [resultsInFront, setResultsInFront] = useState(false)
  const showResults = !!query && (!open || resultsInFront)
  const [adding, setAdding] = useState<'new' | 'upload' | null>(null)
  const [deleted, setDeleted] = useState<{ path: string; versioned: boolean } | null>(null)
  // A document is being edited: its Save is the page's one primary action, so New steps back.
  const [editing, setEditing] = useState(false)
  const canWrite = space?.access === 'write'
  const t = useT()

  const show = (path: string, editing = false, fromTree = false) => {
    setOpen({ path, editing, fromTree })
    setResultsInFront(false)
    setAdding(null)
    setDeleted(null)
  }

  // When what had the keyboard goes (Back, a delete, the results closed or brought forward, a
  // result opened), it goes somewhere that makes sense rather than to the top of the page: the
  // notice of the delete, the results, the document, the document in the tree it came from, or
  // the page's title.
  const side = useRef<HTMLDivElement>(null)
  const main = useRef<HTMLDivElement>(null)
  const backFrom = useRef<string | null>(null)
  const mounted = useRef(false)
  // biome-ignore lint/correctness/useExhaustiveDependencies: what it looks for is in the page, so each of these changes is a reason to run
  useLayoutEffect(() => {
    const path = backFrom.current
    backFrom.current = null
    if (!mounted.current) {
      mounted.current = true
      return
    }
    if (!focusLost()) return
    const notice = main.current?.querySelector<HTMLElement>('[data-deleted]')
    const item = [...(side.current?.querySelectorAll<HTMLElement>('[data-path]') ?? [])].find(
      (el) => el.dataset.path === path,
    )
    const to =
      notice?.querySelector<HTMLElement>('button') ??
      notice ??
      (showResults ? main.current?.querySelector<HTMLElement>('[data-results-title]') : null) ??
      (open ? main.current?.querySelector<HTMLElement>('article h2') : null) ??
      item ??
      side.current?.querySelector<HTMLElement>('h1')
    to?.focus()
  }, [open, query, deleted, showResults])

  const loadingList = (spaces.data === undefined || !!base) && docs.data === undefined && !docs.error && !spaces.error
  const empty = docs.data?.length === 0
  // The empty space's own way in; the page's one primary New stays in the side's head.
  const addButtons = (
    <>
      <Button icon="plus" onClick={() => setAdding('new')}>
        {t('knowledge.new')}
      </Button>
      <Button icon="upload" onClick={() => setAdding('upload')}>
        {t('knowledge.upload')}
      </Button>
    </>
  )

  // The notice after a delete, in whichever pane shows, with its Restore.
  const deletedNotice = base && deleted && (
    <div data-deleted tabIndex={-1} className="kb-deleted">
      <Deleted
        base={base}
        path={deleted.path}
        versioned={deleted.versioned}
        canWrite={canWrite}
        onRestored={(path) => {
          docs.reload()
          show(path)
        }}
      />
    </div>
  )

  return (
    <div className="kb-frame">
      <div className="kb" data-open={open || query ? 'main' : 'list'}>
        <div className="kb-side" ref={side}>
          <div className="kb-side-head">
            <Heading level={1} size="title" tabIndex={-1}>
              {t('shell.sections.knowledge')}
            </Heading>
            {base && canWrite && (
              <>
                <IconButton icon="upload" size="sm" label={t('knowledge.upload')} onClick={() => setAdding('upload')} />
                <Button
                  variant={editing ? 'secondary' : 'primary'}
                  size="sm"
                  icon="plus"
                  onClick={() => setAdding('new')}
                >
                  {t('knowledge.new')}
                </Button>
              </>
            )}
          </div>
          {spaces.data && spaces.data.length > 1 && (
            <Select
              aria-label={t('knowledge.spaces.label')}
              value={space?.id}
              onChange={(e) => {
                setOpen(null)
                setAdding(null)
                setDeleted(null)
                choose(Number(e.target.value))
              }}
            >
              {spaces.data.map((s) => (
                <option key={s.id} value={s.id}>
                  {spaceLabel(s)}
                </option>
              ))}
            </Select>
          )}
          {spaces.data && spaces.data.length > 0 && (
            <SearchBox
              space={space}
              onSearch={(q) => {
                setQuery((last) => ({ ...q, n: (last?.n ?? 0) + 1 }))
                setResultsInFront(true)
              }}
            />
          )}
          {spaces.error && (
            <Notice
              tone="danger"
              title={t('knowledge.errors.spaces')}
              action={
                <Button size="sm" onClick={spaces.reload}>
                  {t('app.tryAgain')}
                </Button>
              }
            >
              {spaces.error}
            </Notice>
          )}
          {docs.error && (
            <Notice
              tone="danger"
              title={t('knowledge.errors.documents')}
              action={
                <Button size="sm" onClick={docs.reload}>
                  {t('app.tryAgain')}
                </Button>
              }
            >
              {docs.error}
            </Notice>
          )}
          {spaces.data?.length === 0 && <EmptyState title={t('knowledge.noSpaces')} />}
          <DocList
            key={base ?? ''}
            docs={docs.data}
            loading={loadingList}
            current={open?.path}
            onOpen={(path) => {
              setQuery(null)
              show(path, false, true)
            }}
          />
        </div>
        <div className="kb-main" ref={main}>
          {query && showResults && (
            <Page width="list">
              {deletedNotice}
              <SearchResults
                key={query.n}
                workspace={workspace}
                space={space}
                query={query}
                onOpen={(r) => {
                  if (r.space_id !== space?.id) choose(r.space_id)
                  show(r.path)
                }}
                onClose={() => {
                  setQuery(null)
                  setResultsInFront(false)
                }}
              />
            </Page>
          )}
          <Page width="reading" hidden={showResults}>
            {!showResults && deletedNotice}
            {base && open ? (
              <DocView
                key={`${base}/${open.path}`}
                base={base}
                path={open.path}
                space={space ? spaceLabel(space) : ''}
                canWrite={canWrite}
                admin={admin && canWrite}
                editing={open.editing ?? false}
                takeFocus={!open.fromTree}
                onEditing={setEditing}
                back={query ? 'results' : 'list'}
                onBack={() => {
                  // To the results: in front of the document, which is kept as it is.
                  if (query) return setResultsInFront(true)
                  backFrom.current = open.path
                  setOpen(null)
                }}
                onOpen={(path) => show(path)}
                onChanged={docs.reload}
                onDeleted={(doc) => {
                  docs.reload()
                  setOpen(null)
                  setDeleted({ path: doc.path, versioned: doc.versioned !== false })
                }}
              />
            ) : empty ? (
              <div className="kb-empty">
                <EmptyState
                  icon={<Icon name="knowledge" size={24} />}
                  title={t('knowledge.empty')}
                  actions={canWrite ? addButtons : undefined}
                >
                  {canWrite ? t('knowledge.emptyHelp') : null}
                </EmptyState>
              </div>
            ) : (
              docs.data && <EmptyState icon={<Icon name="file" size={24} />} title={t('knowledge.pick')} />
            )}
          </Page>
        </div>
        {base && adding === 'new' && (
          <NewDoc
            base={base}
            onCreated={(path) => {
              docs.reload()
              setQuery(null)
              show(path, true)
            }}
            onCancel={() => setAdding(null)}
          />
        )}
        {base && adding === 'upload' && (
          <Upload
            base={base}
            onDone={(path) => {
              docs.reload()
              setQuery(null)
              show(path)
            }}
            onCancel={() => setAdding(null)}
          />
        )}
      </div>
    </div>
  )
}
