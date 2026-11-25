// The knowledge base: a space's documents beside the open one, and search. Writers create,
// edit, upload, move, delete and restore; viewers only read. Every change names the version
// it was based on, so a document someone changed meanwhile is a conflict to settle (reload,
// or overwrite on purpose), never a silent overwrite. On a phone it is the list or the
// document, with a way back.

// The page composes the parts: the list (DocList) or search results beside the open document
// (DocView: the reader, Editor, History, Move, ConfirmDelete), the forms that add a document
// (NewDoc, Upload), and the notice after a delete (Deleted). docs.ts has the data.

import { useState } from 'react'
import { ws } from '../api'
import { useT } from '../i18n'
import { parseDocumentItem } from '../router'
import { SearchBox, type SearchQuery, SearchResults } from '../search/Search'
import { Icon } from '../shell/Icon'
import { SpacePicker, useSpaces } from '../spaces'
import { Deleted } from './Delete'
import { DocList } from './DocList'
import { DocView } from './DocView'
import { useDocs, useLatest } from './docs'
import { GuardContext, useGuard } from './guard'
import { NewDoc } from './NewDoc'
import { Upload } from './Upload'

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
  const [open, setOpen] = useState<{ path: string; editing?: boolean } | null>(
    target?.path ? { path: target.path } : null,
  )
  // Each search counts, so asking the same again searches again rather than showing the last.
  const [query, setQuery] = useState<(SearchQuery & { n: number }) | null>(null)
  const [adding, setAdding] = useState<'new' | 'upload' | null>(null)
  // The notice after a delete belongs to the space and document it was made for.
  const [deleted, setDeleted] = useState<{ base: string; path: string; versioned: boolean } | null>(null)
  const canWrite = space?.access === 'write'
  const t = useT()
  // Opening another document, or going back, asks first when a draft would be lost.
  const guard = useGuard()
  const { ask } = guard
  // What is shown now: a delete or a restore that finishes after the person went to another
  // space or document is not theirs to show there.
  const now = useLatest({ base, path: open?.path })

  const show = (path: string, editing = false) => {
    setOpen({ path, editing })
    setAdding(null)
    setDeleted(null)
  }
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

  return (
    <div className="tt-section">
      <div className="tt-pane-head">
        <h1>{t('shell.sections.knowledge')}</h1>
        {spaces.data && (
          <SpacePicker
            spaces={spaces.data}
            space={space}
            choose={(id) =>
              ask(() => {
                setOpen(null)
                setAdding(null)
                setDeleted(null)
                choose(id)
              })
            }
          />
        )}
        {base && canWrite && (
          <>
            <button type="button" className="tt-button tt-primary" onClick={() => setAdding('new')}>
              <Icon name="plus" />
              {t('knowledge.new')}
            </button>
            <button type="button" className="tt-button" onClick={() => setAdding('upload')}>
              {t('knowledge.upload')}
            </button>
          </>
        )}
      </div>
      {spaces.data && spaces.data.length > 0 && (
        <SearchBox space={space} onSearch={(q) => setQuery((last) => ({ ...q, n: (last?.n ?? 0) + 1 }))} />
      )}
      {(spaces.error || docs.error) && (
        <p role="alert" className="tt-error">
          {spaces.error ?? docs.error}
        </p>
      )}
      {spaces.data?.length === 0 && <p className="tt-muted">{t('knowledge.noSpaces')}</p>}
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
      {base && deleted && deleted.base === base && (
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
      )}
      <GuardContext.Provider value={guard}>
        <div className="tt-split" data-open={open ? 'doc' : 'list'}>
          <div className="tt-kb-side">
            {query ? (
              <SearchResults
                key={query.n}
                workspace={workspace}
                space={space}
                query={query}
                onOpen={(r) => {
                  ask(() => {
                    if (r.space_id !== space?.id) choose(r.space_id)
                    show(r.path)
                  })
                }}
                onClose={() => setQuery(null)}
              />
            ) : (
              <DocList
                docs={docs.data}
                loading={
                  (spaces.data === undefined || !!base) && docs.data === undefined && !docs.error && !spaces.error
                }
                current={open?.path}
                onOpen={(path) => ask(() => show(path))}
              />
            )}
          </div>
          {base && open && (
            <DocView
              key={`${base}/${open.path}`}
              base={base}
              path={open.path}
              canWrite={canWrite}
              admin={admin && canWrite}
              editing={open.editing ?? false}
              onBack={() => setOpen(null)}
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
          )}
        </div>
      </GuardContext.Provider>
    </div>
  )
}
