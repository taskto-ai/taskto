// Searching the knowledge base (docs/api.md, Search): a box that searches the space shown or
// every space of the workspace, and the results with the server's notice, which says when
// a search is full text only (no embeddings model, no consent, the model didn't answer), or
// what it can't find yet (documents still being embedded, an index catching up). The notice
// is said from its codes, in the user's language; the server's words are the fallback.

import { type FormEvent, useRef, useState } from 'react'
import { api, errorText, type Space, ws } from '../api'
import { type Key, type T, useT } from '../i18n'
import { useLoad } from '../load'
import { spaceLabel } from '../spaces'
import { cleanSnippet, isMarkdownPath } from './snippet'

export type SearchQuery = { q: string; scope: 'space' | 'workspace' }

export type SearchResult = {
  space_id: number
  space_name: string
  path: string
  title: string
  snippet: { text: string; match?: boolean }[]
  score: number
  match: 'text' | 'semantic' | 'both'
  version: string
  commit: string
}

type Notice = { code: string; count?: number }

type SearchPage = {
  results: SearchResult[]
  mode: 'hybrid' | 'fulltext'
  notice?: string
  notices?: Notice[]
  next_offset?: number
}

const NOTICES: Record<string, Key> = {
  embeddings_off: 'search.notice.embeddings_off',
  embeddings_failed: 'search.notice.embeddings_failed',
  egress_refused: 'search.notice.egress_refused',
  no_consent: 'search.notice.no_consent',
  pending: 'search.notice.pending',
  behind: 'search.notice.behind',
}

// A page's notice in the user's language, or the server's words if it sent a code this
// version doesn't know (or none).
export function noticeText(t: T, page: Pick<SearchPage, 'notice' | 'notices'>): string | undefined {
  const notices = page.notices ?? []
  if (notices.length === 0 || notices.some((n) => !(n.code in NOTICES))) return page.notice
  return notices.map((n) => t(NOTICES[n.code], { count: n.count ?? 0 })).join(' ')
}

export function SearchBox({ space, onSearch }: { space: Space | undefined; onSearch: (query: SearchQuery) => void }) {
  const [q, setQ] = useState('')
  const [scope, setScope] = useState<SearchQuery['scope']>('space')
  const t = useT()
  const submit = (e: FormEvent) => {
    e.preventDefault()
    if (q.trim()) onSearch({ q: q.trim(), scope: space ? scope : 'workspace' })
  }
  return (
    <search>
      <form className="tt-search" onSubmit={submit}>
        <input
          type="search"
          aria-label={t('search.box.label')}
          placeholder={t('search.box.placeholder')}
          value={q}
          maxLength={500}
          onChange={(e) => setQ(e.target.value)}
        />
        {space && (
          <select
            aria-label={t('search.box.where')}
            value={scope}
            onChange={(e) => setScope(e.target.value as SearchQuery['scope'])}
          >
            <option value="space">{t('search.box.inSpace', { space: spaceLabel(space) })}</option>
            <option value="workspace">{t('search.box.everywhere')}</option>
          </select>
        )}
        <button type="submit" className="tt-button" disabled={!q.trim()}>
          {t('search.box.submit')}
        </button>
      </form>
    </search>
  )
}

export function SearchResults({
  workspace,
  space,
  query,
  onOpen,
  onClose,
}: {
  workspace: number
  space: Space | undefined
  query: SearchQuery
  onOpen: (result: SearchResult) => void
  onClose: () => void
}) {
  const url =
    query.scope === 'space' && space
      ? `${ws(workspace)}/spaces/${space.id}/search?q=${encodeURIComponent(query.q)}`
      : `${ws(workspace)}/search?q=${encodeURIComponent(query.q)}`
  const first = useLoad(() => api<SearchPage>(url), [url])
  return <ResultPages key={url} url={url} first={first} onOpen={onOpen} onClose={onClose} query={query} />
}

function ResultPages({
  url,
  first,
  query,
  onOpen,
  onClose,
}: {
  url: string
  first: { data: SearchPage | undefined; error: string | null }
  query: SearchQuery
  onOpen: (result: SearchResult) => void
  onClose: () => void
}) {
  const [more, setMore] = useState<SearchPage[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const pages = first.data ? [first.data, ...more] : []
  const results = pages.flatMap((p) => p.results)
  const last = pages.at(-1)
  const t = useT()
  // The notice of the latest page: the index may have caught up meanwhile.
  const notice = last && noticeText(t, last)

  // One page at a time, also for two clicks in the same tick (as the history's Older).
  const loadingMore = useRef(false)
  const loadMore = async () => {
    if (last?.next_offset === undefined || loadingMore.current) return
    loadingMore.current = true
    setLoading(true)
    setError(null)
    try {
      const page = await api<SearchPage>(`${url}&offset=${last.next_offset}`)
      setMore((m) => [...m, page])
    } catch (e) {
      setError(errorText(e))
    } finally {
      loadingMore.current = false
      setLoading(false)
    }
  }

  return (
    <section className="tt-search-results" aria-label={t('search.results.label')}>
      <div className="tt-pane-head">
        <h2>
          {query.scope === 'workspace'
            ? t.rich('search.results.titleEverywhere', {
                q: query.q,
                muted: (text) => <span className="tt-muted">{text}</span>,
              })
            : t('search.results.title', { q: query.q })}
        </h2>
        <button type="button" className="tt-button" onClick={onClose}>
          {t('search.results.close')}
        </button>
      </div>
      {notice && (
        <p role="status" className="tt-notice">
          {notice}
        </p>
      )}
      {(first.error || error) && (
        <p role="alert" className="tt-error">
          {first.error ?? error}
        </p>
      )}
      {!first.data && !first.error && <p className="tt-muted">{t('search.results.searching')}</p>}
      {first.data && results.length === 0 && <p className="tt-muted">{t('search.results.none')}</p>}
      {results.length > 0 && (
        <ul className="tt-list">
          {results.map((r) => (
            <li key={`${r.space_id}/${r.path}`}>
              <button type="button" onClick={() => onOpen(r)}>
                <span className="tt-list-title">{r.title || r.path}</span>
                <span className="tt-muted tt-small tt-list-title">
                  {query.scope === 'workspace'
                    ? t('search.results.inSpace', { space: r.space_name, path: r.path })
                    : r.path}
                </span>
                <Snippet pieces={r.snippet} path={r.path} />
              </button>
            </li>
          ))}
        </ul>
      )}
      {last?.next_offset !== undefined && (
        <button type="button" className="tt-button" onClick={loadMore} disabled={loading}>
          {t('search.results.more')}
        </button>
      )}
    </section>
  )
}

// What the document says around the match, as running text: its Markdown marks are left out for
// display (the server's result is unchanged). Plain text in pieces, never markup; the matches are
// marked by the element.
function Snippet({ pieces, path }: { pieces: SearchResult['snippet']; path: string }) {
  // Only a Markdown document has Markdown marks; an upload's path or title is shown as it is.
  const shown = cleanSnippet(pieces, { markdown: isMarkdownPath(path) })
  if (shown.length === 0) return null
  return (
    <span className="tt-snippet tt-small">
      {shown.map((s, i) =>
        // biome-ignore lint/suspicious/noArrayIndexKey: pieces have no identity but their place
        s.match ? <mark key={i}>{s.text}</mark> : <span key={i}>{s.text}</span>,
      )}
    </span>
  )
}
