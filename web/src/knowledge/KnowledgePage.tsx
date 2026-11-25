// The knowledge base: a space's documents beside the open one, and search. Writers create,
// edit, upload, move, delete and restore; viewers only read. Every change names the version
// it was based on, so a document someone changed meanwhile is a conflict to settle (reload,
// or overwrite on purpose), never a silent overwrite. On a phone it is the list or the
// document, with a way back.

// The page composes the parts: beside the tree of documents (DocList), with the space and the
// search box above it, the open document (DocView: the reader, Editor, History, Move,
// ConfirmDelete) or the search results; the dialogs that add a document (NewDoc, Upload), and
// the notice after a delete (Deleted). docs.ts has the data.

import { useLayoutEffect, useMemo, useRef, useState } from 'react'
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
import { useDocs, useLatest } from './docs'
import { focusLost } from './focus'
import { type Guard, GuardContext, useGuard } from './guard'
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
  // The notice after a delete belongs to the space and document it was made for.
  const [deleted, setDeleted] = useState<{ base: string; path: string; versioned: boolean } | null>(null)
  // A document is being edited: its Save is the page's one primary action, so New steps back.
  const [editing, setEditing] = useState(false)
  const canWrite = space?.access === 'write'
  const t = useT()
  // Opening another document, or going back, asks first when a draft would be lost. A search
  // doesn't: its results come in front of the document, which keeps its draft behind them.
  const guard = useGuard()
  const { ask } = guard
  // What is shown now: a delete or a restore that finishes after the person went to another
  // space or document is not theirs to show there.
  const now = useLatest({ base, path: open?.path })
  // While the question about a draft is open, the document it is about is in front (it may have
  // been behind the results); "Keep editing" leaves it there, to be typed in.
  const asking = guard.asking !== null
  const pageGuard = useMemo<Guard>(
    () => ({
      ...guard,
      keep: () => {
        setResultsInFront(false)
        guard.keep()
      },
    }),
    [guard],
  )

  const show = (path: string, editing = false, fromTree = false) => {
    setOpen({ path, editing, fromTree })
    setResultsInFront(false)
    setAdding(null)
    setDeleted(null)
  }
  // Opening the document that is open replaces nothing: no question. Any other asks first.
  const openDoc = (path: string, go: () => void) => (path === now.current.path ? go() : ask(go))
  // The list of a space is read again when something changed in it, if it is the one shown.
  const listChanged = (changed: string) => {
    if (now.current.base === changed) docs.reload()
  }
  // A form or the notice has done its work: it closes, and its document opens unless the person
  // chooses to keep the draft they are writing.
  const finished = (path: string, editing = false) => {
    setAdding(null)
    setDeleted(null)
    ask(() => show(path, editing))
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
  const deletedNotice = base && deleted && deleted.base === base && (
    <div data-deleted tabIndex={-1} className="kb-deleted">
      <Deleted
        key={`${deleted.base}/${deleted.path}`}
        base={deleted.base}
        path={deleted.path}
        versioned={deleted.versioned}
        canWrite={canWrite}
        onChanged={() => listChanged(deleted.base)}
        onRestored={(path) => {
          if (now.current.base === deleted.base) finished(path)
        }}
      />
    </div>
  )

  return (
    <GuardContext.Provider value={pageGuard}>
      <div className="kb-frame">
        <div className="kb" data-open={open || query ? 'main' : 'list'}>
          <div className="kb-side" ref={side}>
            <div className="kb-side-head">
              <Heading level={1} size="title" tabIndex={-1}>
                {t('shell.sections.knowledge')}
              </Heading>
              {base && canWrite && (
                <>
                  <IconButton
                    icon="upload"
                    size="sm"
                    label={t('knowledge.upload')}
                    onClick={() => setAdding('upload')}
                  />
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
                  const id = Number(e.target.value)
                  ask(() => {
                    setOpen(null)
                    setAdding(null)
                    setDeleted(null)
                    choose(id)
                  })
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
              onOpen={(path) =>
                openDoc(path, () => {
                  setQuery(null)
                  show(path, false, true)
                })
              }
            />
          </div>
          <div className="kb-main" ref={main}>
            {/* Kept while a document is in front of them, so Back to the results finds every page
                that was loaded, where it was. */}
            {query && (
              <Page width="list" hidden={!showResults || asking}>
                {showResults && deletedNotice}
                <SearchResults
                  key={query.n}
                  workspace={workspace}
                  space={space}
                  query={query}
                  onOpen={(r) => {
                    const go = () => {
                      if (r.space_id !== space?.id) choose(r.space_id)
                      show(r.path)
                    }
                    if (r.space_id === space?.id) openDoc(r.path, go)
                    else ask(go)
                  }}
                  onClose={() => {
                    setQuery(null)
                    setResultsInFront(false)
                  }}
                />
              </Page>
            )}
            <Page width="reading" hidden={showResults && !asking}>
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
                    // The delete finished after the person went to another space: nothing to show.
                    if (now.current.base !== base) return
                    docs.reload()
                    // Another document may be open by now; that one stays.
                    if (now.current.path === doc.path) {
                      ask(() => {
                        if (now.current.base === base && now.current.path === doc.path) setOpen(null)
                      }, 'deleted')
                    }
                    setDeleted({ base, path: doc.path, versioned: doc.versioned !== false })
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
              onCreated={(path) => finished(path, true)}
              onChanged={() => listChanged(base)}
              onCancel={() => setAdding(null)}
            />
          )}
          {base && adding === 'upload' && (
            <Upload
              base={base}
              onDone={(path) => finished(path)}
              onChanged={() => listChanged(base)}
              onCancel={() => setAdding(null)}
            />
          )}
        </div>
      </div>
    </GuardContext.Provider>
  )
}
