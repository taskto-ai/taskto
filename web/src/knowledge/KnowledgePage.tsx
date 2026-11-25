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
import { useDocs } from './docs'
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
  const [deleted, setDeleted] = useState<{ path: string; versioned: boolean } | null>(null)
  const canWrite = space?.access === 'write'
  const t = useT()

  const show = (path: string, editing = false) => {
    setOpen({ path, editing })
    setAdding(null)
    setDeleted(null)
  }

  return (
    <div className="tt-section">
      <div className="tt-pane-head">
        <h1>{t('shell.sections.knowledge')}</h1>
        {spaces.data && (
          <SpacePicker
            spaces={spaces.data}
            space={space}
            choose={(id) => {
              setOpen(null)
              setAdding(null)
              setDeleted(null)
              choose(id)
            }}
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
          onCreated={(path) => {
            docs.reload()
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
            show(path)
          }}
          onCancel={() => setAdding(null)}
        />
      )}
      {base && deleted && (
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
      )}
      <div className="tt-split" data-open={open ? 'doc' : 'list'}>
        <div className="tt-kb-side">
          {query ? (
            <SearchResults
              key={query.n}
              workspace={workspace}
              space={space}
              query={query}
              onOpen={(r) => {
                if (r.space_id !== space?.id) choose(r.space_id)
                show(r.path)
              }}
              onClose={() => setQuery(null)}
            />
          ) : (
            <DocList
              docs={docs.data}
              loading={(spaces.data === undefined || !!base) && docs.data === undefined && !docs.error && !spaces.error}
              current={open?.path}
              onOpen={(path) => show(path)}
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
              docs.reload()
              setOpen(null)
              setDeleted({ path: doc.path, versioned: doc.versioned !== false })
            }}
          />
        )}
      </div>
    </div>
  )
}
