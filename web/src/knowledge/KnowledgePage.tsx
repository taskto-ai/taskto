// The knowledge base: a space's documents beside the open one, and search. Writers create,
// edit, upload, move, delete and restore; viewers only read. Every change names the version
// it was based on, so a document someone changed meanwhile is a conflict to settle (reload,
// or overwrite on purpose), never a silent overwrite. On a phone it is the list or the
// document, with a way back.

import { type FormEvent, useRef, useState } from 'react'
import { ApiError, api, type Doc, errorText, ws } from '../api'
import { has, type T, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { type LinkHandler, Markdown } from '../markdown'
import { parseDocumentItem } from '../router'
import { SearchBox, type SearchQuery, SearchResults } from '../search/Search'
import { Icon } from '../shell/Icon'
import { SpacePicker, useSpaces } from '../spaces'

type Change = { commit: string; doc?: Doc }

// The address the server gives the author of a commit whose account was deleted (internal/knowledge,
// DeletedAuthorEmail): shown translated, not as the name stored in git.
const DELETED_AUTHOR = 'deleted-user@deleted.invalid'
const authorName = (a: { name: string; email: string }, t: T) =>
  a.email === DELETED_AUTHOR ? t('common.deletedUser') : a.name

export type Commit = {
  commit: string
  author: { name: string; email: string }
  agent?: string
  time: string
  message: string
  change?: 'create' | 'update' | 'delete'
  version?: string
  // false for a version whose bytes aren't kept: an unversioned file's, or one removed
  // from history.
  versioned?: boolean
}

export const encodePath = (path: string) => path.split('/').map(encodeURIComponent).join('/')

const isConflict = (e: unknown) => e instanceof ApiError && e.status === 409
// A conflict the server named (doc_changed, doc_exists…): its message says what happened.
const isNamedConflict = (e: unknown) =>
  e instanceof ApiError && e.status === 409 && !!e.code && e.code !== 'doc_conflict' && has(`errors.${e.code}`)
const isMarkdown = (path: string) => /\.md$/i.test(path)
// Audio and video the browser can play, by extension (the server sends their media type).
const isVideo = (path: string) => /\.(mp4|m4v|mov|webm|ogv)$/i.test(path)
const isAudio = (path: string) => /\.(mp3|m4a|aac|wav|flac|ogg|oga|opus|weba)$/i.test(path)

// Every page of the list: a space has at most a few thousand documents.
async function listDocs(base: string): Promise<Doc[]> {
  const docs: Doc[] = []
  let after = ''
  do {
    const page = await api<{ docs: Doc[]; next?: string }>(
      `${base}/docs?limit=1000${after ? `&after=${encodeURIComponent(after)}` : ''}`,
    )
    docs.push(...page.docs)
    after = page.next ?? ''
  } while (after)
  return docs
}

// A link in a document to another one (relative to its folder) opens it here; null for a
// link that isn't one, or leaves the space.
export function resolveDocLink(from: string, href: string): string | null {
  if (/^([a-z][a-z0-9+.-]*:|\/|#|\?)/i.test(href)) return null
  let target: string
  try {
    target = decodeURIComponent(href.replace(/[?#].*$/, ''))
  } catch {
    return null
  }
  const parts = from.split('/').slice(0, -1)
  for (const part of target.split('/')) {
    if (part === '..') {
      if (parts.length === 0) return null
      parts.pop()
    } else if (part !== '.' && part !== '') parts.push(part)
  }
  return parts.length > 0 ? parts.join('/') : null
}

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
  const docs = useLoad(() => (base ? listDocs(base) : Promise.resolve(undefined)), [base])
  const [open, setOpen] = useState<{ path: string; editing?: boolean } | null>(
    target?.path ? { path: target.path } : null,
  )
  // Each search counts, so asking the same again searches again rather than showing the last.
  const [query, setQuery] = useState<(SearchQuery & { n: number }) | null>(null)
  const [adding, setAdding] = useState<'new' | 'upload' | null>(null)
  const [deleted, setDeleted] = useState<{ path: string; versioned: boolean } | null>(null)
  const canWrite = space?.access === 'write'
  const t = useT()
  const f = useFormat()

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
            <>
              {(spaces.data === undefined || base) && docs.data === undefined && !docs.error && !spaces.error && (
                <p className="tt-muted">{t('app.loading')}</p>
              )}
              {docs.data?.length === 0 && <p className="tt-muted">{t('knowledge.empty')}</p>}
              {docs.data && docs.data.length > 0 && (
                <ul className="tt-list" aria-label={t('knowledge.documents')}>
                  {docs.data.map((d) => (
                    <li key={d.path}>
                      <button
                        type="button"
                        aria-current={d.path === open?.path || undefined}
                        onClick={() => show(d.path)}
                      >
                        <span className="tt-list-title">{d.path}</span>
                        <span className="tt-muted tt-small">
                          {d.versioned === false
                            ? t('knowledge.sizeUnversioned', { size: f.bytes(d.size) })
                            : f.bytes(d.size)}
                        </span>
                      </button>
                    </li>
                  ))}
                </ul>
              )}
            </>
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

function NewDoc({
  base,
  onCreated,
  onCancel,
}: {
  base: string
  onCreated: (path: string) => void
  onCancel: () => void
}) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const t = useT()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    const trimmed = name.trim().replace(/^\/+/, '')
    if (!trimmed) return
    // A new document is Markdown; other files are uploaded.
    const path = isMarkdown(trimmed) ? trimmed : `${trimmed}.md`
    const title = (path.split('/').pop() ?? path).replace(/\.md$/i, '')
    setBusy(true)
    setError(null)
    try {
      await api<Change>(`${base}/docs/${encodePath(path)}`, { method: 'PUT', body: { content: `# ${title}\n` } })
      onCreated(path)
    } catch (err) {
      setError(isConflict(err) ? t('knowledge.newDoc.exists', { path }) : errorText(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form className="tt-panel tt-form" onSubmit={submit} aria-label={t('knowledge.newDoc.label')}>
      <label>
        {t('knowledge.newDoc.name')}
        <input
          value={name}
          placeholder={t('knowledge.newDoc.placeholder')}
          onChange={(e) => setName(e.target.value)}
          // biome-ignore lint/a11y/noAutofocus: the form was opened to type this
          autoFocus
        />
      </label>
      <span className="tt-hint">{t('knowledge.newDoc.hint')}</span>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      <div className="tt-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || !name.trim()}>
          {t('knowledge.newDoc.create')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}

// A Markdown file is sent as its text, checked here to be UTF-8; any other file as its bytes,
// streamed by the browser whatever its size.
async function markdownText(file: File, notUtf8: string): Promise<string> {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(await file.arrayBuffer())
  } catch {
    throw new Error(notUtf8)
  }
}

function Upload({ base, onDone, onCancel }: { base: string; onDone: (path: string) => void; onCancel: () => void }) {
  const [file, setFile] = useState<File | null>(null)
  const [path, setPath] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [exists, setExists] = useState(false)
  const target = path.trim().replace(/^\/+/, '')
  const t = useT()
  const f = useFormat()

  // Without `replace`, it only creates: a file already there is the user's to replace.
  const send = async (replace: boolean) => {
    if (!file || !target) return
    setBusy(true)
    setError(null)
    try {
      const url = `${base}/docs/${encodePath(target)}`
      const text = isMarkdown(target)
        ? await markdownText(file, t('knowledge.uploadForm.notUtf8', { name: file.name }))
        : null
      // Gone since it was found: replacing it is creating it.
      const current = replace
        ? await api<Doc>(url).catch((e: unknown) => {
            if (e instanceof ApiError && e.status === 404) return null
            throw e
          })
        : null
      if (text !== null) {
        const body = { content: text }
        await api<Change>(url, { method: 'PUT', body: current ? { ...body, base_version: current.version } : body })
      } else {
        const query = current ? `?base_version=${current.version}` : ''
        await api<Change>(`${url}${query}`, { method: 'PUT', bytes: file })
      }
      onDone(target)
    } catch (err) {
      setExists(isConflict(err) && !replace)
      setError(
        isConflict(err)
          ? replace
            ? t('knowledge.uploadForm.changed', { path: target })
            : t('knowledge.uploadForm.exists', { path: target })
          : errorText(err),
      )
    } finally {
      setBusy(false)
    }
  }

  return (
    <form
      className="tt-panel tt-form"
      aria-label={t('knowledge.uploadForm.label')}
      onSubmit={(e) => {
        e.preventDefault()
        send(false)
      }}
    >
      <label>
        {t('knowledge.uploadForm.file')}
        <input
          type="file"
          onChange={(e) => {
            const f = e.target.files?.[0] ?? null
            setFile(f)
            setExists(false)
            setError(null)
            if (f) setPath(f.name)
          }}
        />
      </label>
      <label>
        {t('knowledge.uploadForm.saveAs')}
        <input
          value={path}
          onChange={(e) => {
            setPath(e.target.value)
            setExists(false)
          }}
        />
      </label>
      <span className="tt-hint">{t('knowledge.uploadForm.hint')}</span>
      {busy && file && (
        <p className="tt-muted" role="status">
          {t('knowledge.uploadForm.sending', { name: file.name, size: f.bytes(file.size) })}
        </p>
      )}
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      <div className="tt-actions">
        {exists ? (
          <button type="button" className="tt-button tt-danger" disabled={busy} onClick={() => send(true)}>
            {t('knowledge.uploadForm.replace')}
          </button>
        ) : (
          <button type="submit" className="tt-button tt-primary" disabled={busy || !file || !target}>
            {t('knowledge.uploadForm.submit')}
          </button>
        )}
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}

// After a delete: the document can come back as it was, from its history.
function Deleted({
  base,
  path,
  versioned,
  canWrite,
  onRestored,
}: {
  base: string
  path: string
  versioned: boolean
  canWrite: boolean
  onRestored: (path: string) => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const t = useT()
  const restore = async () => {
    setBusy(true)
    setError(null)
    try {
      const { commits } = await api<{ commits: Commit[] }>(`${base}/history/${encodePath(path)}?limit=10`)
      // Someone made it again since: nothing to bring back, it is there.
      if (commits[0] && commits[0].change !== 'delete') return onRestored(path)
      const before = commits.find((c) => c.change !== 'delete')
      if (!before) throw new Error(t('knowledge.deleted.noEarlier', { path }))
      await api<Change>(`${base}/restore/${encodePath(path)}`, { method: 'POST', body: { commit: before.commit } })
      onRestored(path)
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }
  if (!versioned) {
    return (
      <div className="tt-notice" role="status">
        <span>{t('knowledge.deleted.unversioned', { path })}</span>
      </div>
    )
  }
  return (
    <div className="tt-notice" role="status">
      <span>{t(canWrite ? 'knowledge.deleted.restorable' : 'knowledge.deleted.kept', { path })}</span>
      {canWrite && (
        <button type="button" className="tt-button" onClick={restore} disabled={busy}>
          {t('knowledge.deleted.restore')}
        </button>
      )}
      {error && <span className="tt-error">{error}</span>}
    </div>
  )
}

type Mode = 'view' | 'edit' | 'history' | 'move' | 'delete'

function DocView({
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
  const doc = useLoad(() => api<Doc>(url), [url])
  const [mode, setMode] = useState<Mode>(editing ? 'edit' : 'view')
  const text = doc.data?.content
  const t = useT()
  const onLink = (href: string) => {
    const target = resolveDocLink(path, href)
    return target === null ? null : () => onOpen(target)
  }
  const changed = () => {
    doc.reload()
    onChanged()
    setMode('view')
  }

  return (
    <article className="tt-doc" aria-label={path}>
      <div className="tt-pane-head">
        <button type="button" className="tt-icon-button tt-back" aria-label={t('knowledge.doc.back')} onClick={onBack}>
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
            onOpen(to)
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
          onRestored={changed}
          onReload={doc.reload}
        />
      )}
      {doc.data && (mode === 'view' || mode === 'move' || mode === 'delete') && (
        <Content url={url} doc={doc.data} onLink={onLink} />
      )}
    </article>
  )
}

// A document's text as a preview, or a file's download link, with a player for audio and
// video.
function Content({ url, doc, onLink }: { url: string; doc: Doc; onLink: LinkHandler }) {
  const t = useT()
  const f = useFormat()
  if (doc.content !== undefined) {
    return doc.content.trim() ? (
      <Markdown text={doc.content} onLink={onLink} />
    ) : (
      <p className="tt-muted">{t('knowledge.doc.empty')}</p>
    )
  }
  const raw = `${url}${url.includes('?') ? '&' : '?'}raw=true`
  return (
    <>
      {isVideo(doc.path) && (
        // biome-ignore lint/a11y/useMediaCaption: people's own recordings come without captions
        <video className="tt-media" controls preload="metadata" src={raw} aria-label={doc.path} />
      )}
      {isAudio(doc.path) && (
        // biome-ignore lint/a11y/useMediaCaption: people's own recordings come without captions
        <audio className="tt-media" controls preload="metadata" src={raw} aria-label={doc.path} />
      )}
      <p>
        <a href={raw} download>
          {t('knowledge.doc.download')}
        </a>{' '}
        <span className="tt-muted tt-small">{f.bytes(doc.size)}</span>
      </p>
      {doc.versioned === false && <p className="tt-muted tt-small">{t('knowledge.notVersioned')}</p>}
    </>
  )
}

// A change refused because the document changed meanwhile: say so, and offer the reload.
function Problem({ error, onReload }: { error: unknown; onReload: () => void }) {
  const t = useT()
  if (!isConflict(error)) {
    return (
      <p role="alert" className="tt-error">
        {errorText(error)}
      </p>
    )
  }
  return (
    <div role="alert" className="tt-conflict">
      <span>{isNamedConflict(error) ? errorText(error) : t('knowledge.conflict')}</span>
      <button type="button" className="tt-button" onClick={onReload}>
        {t('knowledge.reload')}
      </button>
    </div>
  )
}

// Editing a Markdown document: the text and its preview. Saving sends the version the edit
// started from; if the document changed (or went away) meanwhile, the edit is kept and the
// writer chooses: take the current text, or save theirs over it.
function Editor({ url, doc, onSaved, onCancel }: { url: string; doc: Doc; onSaved: () => void; onCancel: () => void }) {
  const [text, setText] = useState(doc.content ?? '')
  const [base, setBase] = useState<string | null>(doc.version)
  const [message, setMessage] = useState('')
  const [tab, setTab] = useState<'write' | 'preview'>('write')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // The document as it is now, after a 409; null if it is gone.
  const [conflict, setConflict] = useState<{ current: Doc | null } | null>(null)
  const t = useT()

  const save = async (version: string | null) => {
    setBusy(true)
    setError(null)
    try {
      await api<Change>(url, {
        method: 'PUT',
        body: {
          content: text,
          ...(version ? { base_version: version } : {}),
          ...(message.trim() ? { message: message.trim() } : {}),
        },
      })
      onSaved()
    } catch (err) {
      if (!isConflict(err)) {
        setError(errorText(err))
        return
      }
      try {
        setConflict({ current: await api<Doc>(url) })
      } catch (e) {
        if (e instanceof ApiError && e.status === 404) setConflict({ current: null })
        else setError(t('knowledge.editor.reloadFailed', { error: errorText(e) }))
      }
    } finally {
      setBusy(false)
    }
  }

  const takeTheirs = (current: Doc) => {
    setText(current.content ?? '')
    setBase(current.version)
    setConflict(null)
  }
  const overwrite = (current: Doc | null) => {
    const version = current?.version ?? null
    setBase(version)
    setConflict(null)
    save(version)
  }

  return (
    <form
      className="tt-editor"
      aria-label={t('knowledge.editor.label')}
      onSubmit={(e) => {
        e.preventDefault()
        save(base)
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
      <div className="tt-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || conflict !== null}>
          {t('knowledge.editor.save')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}

function Move({
  base,
  doc,
  onMoved,
  onReload,
  onCancel,
}: {
  base: string
  doc: Doc
  onMoved: (to: string) => void
  onReload: () => void
  onCancel: () => void
}) {
  const [to, setTo] = useState(doc.path)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const target = to.trim().replace(/^\/+/, '')
  const t = useT()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await api<Change>(`${base}/docs/${encodePath(doc.path)}`, {
        method: 'PATCH',
        body: { to: target, base_version: doc.version },
      })
      onMoved(target)
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }
  return (
    <form className="tt-panel tt-form" aria-label={t('knowledge.move.label')} onSubmit={submit}>
      <label>
        {t('knowledge.move.to')}
        <input value={to} onChange={(e) => setTo(e.target.value)} />
      </label>
      {error !== null && (
        <Problem
          error={error}
          onReload={() => {
            setError(null)
            onReload()
          }}
        />
      )}
      <div className="tt-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || !target || target === doc.path}>
          {t('knowledge.move.submit')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}

function ConfirmDelete({
  url,
  doc,
  onDeleted,
  onReload,
  onCancel,
}: {
  url: string
  doc: Doc
  onDeleted: () => void
  onReload: () => void
  onCancel: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const t = useT()
  const remove = async () => {
    setBusy(true)
    setError(null)
    try {
      await api<Change>(url, { method: 'DELETE', body: { base_version: doc.version } })
      onDeleted()
    } catch (err) {
      setError(err)
      setBusy(false)
    }
  }
  return (
    <fieldset className="tt-panel" aria-label={t('knowledge.delete.label')}>
      <p>
        {t(doc.versioned === false ? 'knowledge.delete.questionUnversioned' : 'knowledge.delete.question', {
          path: doc.path,
        })}
      </p>
      {error !== null && (
        <Problem
          error={error}
          onReload={() => {
            setError(null)
            onReload()
          }}
        />
      )}
      <div className="tt-actions">
        <button type="button" className="tt-button tt-danger" onClick={remove} disabled={busy}>
          {t('knowledge.delete.submit')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </fieldset>
  )
}

const CHANGE_LABEL = {
  create: 'knowledge.history.create',
  update: 'knowledge.history.update',
  delete: 'knowledge.history.delete',
} as const

// The commits that changed a document, newest first; any version can be looked at, and
// writers can make the document that version again. An unversioned file's history only says
// who changed it: its older versions' bytes aren't kept, nor those of versions removed from
// history, which an admin can do.
function History({
  base,
  url,
  current,
  canWrite,
  admin,
  onLink,
  onRestored,
  onReload,
}: {
  base: string
  url: string
  current: Doc
  canWrite: boolean
  admin: boolean
  onLink: LinkHandler
  onRestored: () => void
  onReload: () => void
}) {
  const historyUrl = `${base}/history/${encodePath(current.path)}`
  const first = useLoad(() => api<{ commits: Commit[]; next?: string }>(historyUrl), [historyUrl])
  const [removing, setRemoving] = useState(false)
  const [removed, setRemoved] = useState<number | null>(null)
  const [older, setOlder] = useState<{ commits: Commit[]; next?: string }[]>([])
  const [viewing, setViewing] = useState<Commit | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const unversioned = current.versioned === false
  const pages = first.data ? [first.data, ...older] : []
  const commits = pages.flatMap((p) => p.commits)
  const next = pages.at(-1)?.next
  const kept = (c: Commit) => c.versioned !== false
  // Something to remove: an older version still kept, or one not loaded yet.
  const canRemove =
    admin &&
    first.data !== undefined &&
    (next !== undefined || commits.some((c) => c.version && c.version !== current.version && kept(c)))
  const t = useT()
  const f = useFormat()

  // One page at a time: a second click while one loads would add the same page twice.
  const loadingOlder = useRef(false)
  const [loadingMore, setLoadingMore] = useState(false)
  const more = async () => {
    if (!next || loadingOlder.current) return
    loadingOlder.current = true
    setLoadingMore(true)
    try {
      const page = await api<{ commits: Commit[]; next?: string }>(`${historyUrl}?before=${encodeURIComponent(next)}`)
      setOlder((o) => [...o, page])
    } catch (err) {
      setError(err)
    } finally {
      loadingOlder.current = false
      setLoadingMore(false)
    }
  }
  const restore = async (c: Commit) => {
    setBusy(true)
    setError(null)
    try {
      await api<Change>(`${base}/restore/${encodePath(current.path)}`, {
        method: 'POST',
        body: { commit: c.commit, base_version: current.version },
      })
      onRestored()
    } catch (err) {
      setError(err)
      setBusy(false)
    }
  }

  return (
    <section className="tt-history" aria-label={t('knowledge.history.label')}>
      {first.error && (
        <p role="alert" className="tt-error">
          {first.error}
        </p>
      )}
      {!first.data && !first.error && <p className="tt-muted">{t('app.loading')}</p>}
      {unversioned && <p className="tt-muted tt-small">{t('knowledge.history.unversioned')}</p>}
      {error !== null && (
        <Problem
          error={error}
          onReload={() => {
            setError(null)
            onReload()
          }}
        />
      )}
      <ol className="tt-list">
        {commits.map((c) => (
          <li key={c.commit} className="tt-history-entry" aria-current={c === viewing || undefined}>
            <span>
              <strong>{t(c.change ? CHANGE_LABEL[c.change] : 'knowledge.history.changed')}</strong> {c.message}
            </span>
            <span className="tt-muted tt-small">
              {c.agent
                ? t('knowledge.history.byWithAgent', {
                    name: authorName(c.author, t),
                    agent: c.agent,
                    time: f.dateTime(c.time),
                  })
                : t('knowledge.history.by', { name: authorName(c.author, t), time: f.dateTime(c.time) })}
            </span>
            {c.version && unversioned && c.version === current.version && (
              <span className="tt-muted tt-small">{t('knowledge.history.current')}</span>
            )}
            {c.version && !unversioned && !kept(c) && c.version !== current.version && (
              <span className="tt-muted tt-small">{t('knowledge.history.removedVersion')}</span>
            )}
            {c.version && !unversioned && (kept(c) || c.version === current.version) && (
              <span className="tt-actions">
                <button type="button" className="tt-button" onClick={() => setViewing(c)}>
                  {t('knowledge.history.view')}
                </button>
                {c.version === current.version ? (
                  <span className="tt-muted tt-small">{t('knowledge.history.current')}</span>
                ) : (
                  canWrite && (
                    <button type="button" className="tt-button" disabled={busy} onClick={() => restore(c)}>
                      {t('knowledge.history.restore')}
                    </button>
                  )
                )}
              </span>
            )}
          </li>
        ))}
      </ol>
      {next && (
        <button type="button" className="tt-button" disabled={loadingMore} onClick={more}>
          {t('knowledge.history.older')}
        </button>
      )}
      {removed !== null && (
        <p role="status" className="tt-muted tt-small">
          {removed === 0 ? t('knowledge.history.removedNone') : t('knowledge.history.removedCount', { count: removed })}
        </p>
      )}
      {canRemove && !removing && (
        <button type="button" className="tt-button tt-danger" onClick={() => setRemoving(true)}>
          {t('knowledge.history.removeOld')}
        </button>
      )}
      {removing && (
        <ConfirmRemoveOld
          url={historyUrl}
          doc={current}
          onRemoved={(n) => {
            setRemoving(false)
            setRemoved(n)
            // The commits have new IDs: the history is loaded again.
            setViewing(null)
            setOlder([])
            first.reload()
          }}
          onReload={onReload}
          onCancel={() => setRemoving(false)}
        />
      )}
      {viewing && <AtCommit key={viewing.commit} url={url} commit={viewing} onLink={onLink} />}
    </section>
  )
}

// Removing a file's older versions from history, for an admin: it can't be undone, and
// the backups made before still hold them, so the question says both.
function ConfirmRemoveOld({
  url,
  doc,
  onRemoved,
  onReload,
  onCancel,
}: {
  url: string
  doc: Doc
  onRemoved: (count: number) => void
  onReload: () => void
  onCancel: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const t = useT()
  const remove = async () => {
    setBusy(true)
    setError(null)
    try {
      const res = await api<{ commit: string; removed: number }>(url, {
        method: 'DELETE',
        body: { base_version: doc.version },
      })
      onRemoved(res.removed)
    } catch (err) {
      setError(err)
      setBusy(false)
    }
  }
  return (
    <fieldset className="tt-panel" aria-label={t('knowledge.history.removeLabel')}>
      <p>{t('knowledge.history.removeQuestion', { path: doc.path })}</p>
      <p className="tt-small">{t('knowledge.history.removeForGood')}</p>
      <p className="tt-small">{t('knowledge.history.removeBackups')}</p>
      {error !== null && (
        <Problem
          error={error}
          onReload={() => {
            setError(null)
            onReload()
          }}
        />
      )}
      <div className="tt-actions">
        <button type="button" className="tt-button tt-danger" onClick={remove} disabled={busy}>
          {t('knowledge.history.removeSubmit')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </fieldset>
  )
}

function AtCommit({ url, commit, onLink }: { url: string; commit: Commit; onLink: LinkHandler }) {
  const at = `${url}?at=${commit.commit}`
  const doc = useLoad(() => api<Doc>(at), [at])
  const t = useT()
  const f = useFormat()
  const asOf = t('knowledge.history.asOf', { time: f.dateTime(commit.time) })
  return (
    <section className="tt-preview" aria-label={asOf}>
      <p className="tt-muted tt-small">{asOf}</p>
      {doc.error && (
        <p role="alert" className="tt-error">
          {doc.error}
        </p>
      )}
      {doc.data && <Content url={at} doc={doc.data} onLink={onLink} />}
    </section>
  )
}
