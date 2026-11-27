import { useLayoutEffect, useMemo, useRef, useState } from 'react'
import { ws } from '../api'
import { useT } from '../i18n'
import { parseDocumentItem, useBeforeLeave } from '../router'
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
import { DocList, type Reveal } from './DocList'
import { DocView } from './DocView'
import { useDocs, useLatest } from './docs'
import { focusLost } from './focus'
import { type Guard, GuardContext, useGuard } from './guard'
import { LoadingStatus } from './loading'
import { NewDoc } from './NewDoc'
import { Upload } from './Upload'
import './knowledge.css'

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
  const [open, setOpen] = useState<{ path: string; editing?: boolean; fromTree?: boolean } | null>(
    target?.path ? { path: target.path, fromTree: true } : null,
  )
  const [query, setQuery] = useState<(SearchQuery & { n: number }) | null>(null)
  const [resultsInFront, setResultsInFront] = useState(false)
  const showResults = !!query && (!open || resultsInFront)
  const [adding, setAdding] = useState<'new' | 'upload' | null>(null)
  const [deleted, setDeleted] = useState<{ base: string; path: string; versioned: boolean } | null>(null)
  const [editing, setEditing] = useState(false)
  const canWrite = space?.access === 'write'
  const t = useT()
  const guard = useGuard()
  const { ask } = guard
  useBeforeLeave((go) => ask(go))
  const now = useLatest({ base, path: open?.path })
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

  const [reveal, setReveal] = useState<Reveal | null>(null)
  const show = (path: string, editing = false, fromTree = false) => {
    setOpen({ path, editing, fromTree })
    if (!fromTree) setReveal({ path })
    setResultsInFront(false)
    setAdding(null)
    setDeleted(null)
  }
  const openDoc = (path: string, go: () => void) => {
    if (path !== now.current.path) return ask(go)
    guard.forget()
    go()
  }
  const listChanged = (changed: string) => {
    if (now.current.base === changed) docs.reload()
  }
  const finished = (path: string, editing = false) => {
    setAdding(null)
    setDeleted(null)
    ask(() => show(path, editing))
  }

  const side = useRef<HTMLDivElement>(null)
  const main = useRef<HTMLDivElement>(null)
  const backFrom = useRef<string | null>(null)
  const opener = useRef<HTMLElement | null>(null)
  const mounted = useRef(false)
  useLayoutEffect(() => {
    const path = backFrom.current
    backFrom.current = null
    if (!mounted.current) {
      mounted.current = true
      return
    }
    if (!focusLost()) return
    const notice = deleted ? main.current?.querySelector<HTMLElement>('[data-deleted]') : null
    const item = [...(side.current?.querySelectorAll<HTMLElement>('[data-path]') ?? [])].find(
      (el) => el.dataset.path === path,
    )
    const results = query && showResults
    const to =
      notice?.querySelector<HTMLElement>('button') ??
      notice ??
      (results
        ? opener.current?.isConnected
          ? opener.current
          : main.current?.querySelector<HTMLElement>('[data-results-title]')
        : null) ??
      (open ? main.current?.querySelector<HTMLElement>('article h2') : null) ??
      item ??
      side.current?.querySelector<HTMLElement>('h1')
    to?.focus()
  }, [open, query, deleted, showResults])

  const loadingList = (spaces.data === undefined || !!base) && docs.data === undefined && !docs.error && !spaces.error
  const empty = docs.data?.length === 0
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
      <LoadingStatus>
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
                reveal={reveal}
                onOpen={(path) =>
                  openDoc(path, () => {
                    setQuery(null)
                    show(path, false, true)
                  })
                }
              />
            </div>
            <div className="kb-main" ref={main}>
              {deletedNotice}
              {query && (
                <Page width="list" hidden={!showResults || asking}>
                  <SearchResults
                    key={query.n}
                    workspace={workspace}
                    space={space}
                    query={query}
                    onOpen={(r) => {
                      const at = document.activeElement
                      opener.current = at instanceof HTMLElement && main.current?.contains(at) ? at : null
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
                      if (query) return setResultsInFront(true)
                      backFrom.current = open.path
                      setOpen(null)
                    }}
                    onOpen={(path) => show(path)}
                    onChanged={docs.reload}
                    onDeleted={(doc) => {
                      if (now.current.base !== base) return
                      docs.reload()
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
      </LoadingStatus>
    </GuardContext.Provider>
  )
}
