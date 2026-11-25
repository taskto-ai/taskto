// Searching the knowledge base (docs/api.md, Search): a box that searches the space shown or
// every space of the workspace, and the results with the server's notice, which says when
// a search is full text only (no embeddings model, no consent, the model didn't answer), or
// what it can't find yet (documents still being embedded, an index catching up). The notice
// is said from its codes, in the user's language; the server's words are the fallback.
// The same words also search the cards of the boards, the records of the apps and, when the
// whole workspace is searched, the person's own conversations: each comes apart from the
// documents, under a heading of its own, linking to its card, its app or its conversation.

import { type FormEvent, useRef, useState } from 'react'
import { api, errorText, type Space, ws } from '../api'
import { appPath } from '../apps/route'
import { type Key, type T, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { boardItem, Link, routePath } from '../router'
import { spaceLabel } from '../spaces'

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

export type CardResult = {
  space_id: number
  space_name: string
  board_id: number
  board_name: string
  card_id: number
  title: string
  column: 'backlog' | 'todo' | 'doing' | 'review' | 'done' | 'archived'
  snippet: { text: string; match?: boolean }[]
}

type CardPage = { results: CardResult[]; next_offset?: number; incomplete?: boolean }

export type RecordResult = {
  space_id: number
  space_name: string
  app_id: number
  app_name: string
  record_id: number
  snippet: { text: string; match?: boolean }[]
}

type RecordPage = { results: RecordResult[]; next_offset?: number; incomplete?: boolean }

export type ConversationResult = {
  conversation_id: number
  title: string
  archived: boolean
  updated_at: string
  snippet: { text: string; match?: boolean }[]
}

type ConversationPage = { results: ConversationResult[]; next_offset?: number; incomplete?: boolean }

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
  const where = query.scope === 'space' && space ? `${ws(workspace)}/spaces/${space.id}` : ws(workspace)
  const q = encodeURIComponent(query.q)
  const url = `${where}/search?q=${q}`
  const cardsUrl = `${where}/search/cards?q=${q}`
  const recordsUrl = `${where}/search/records?q=${q}`
  // Conversations belong to a person, not to a space: only a search of the whole workspace has them.
  const conversationsUrl = query.scope === 'workspace' || !space ? `${ws(workspace)}/search/conversations?q=${q}` : null
  return (
    <ResultPages
      key={url}
      workspace={workspace}
      url={url}
      cardsUrl={cardsUrl}
      recordsUrl={recordsUrl}
      conversationsUrl={conversationsUrl}
      onOpen={onOpen}
      onClose={onClose}
      query={query}
    />
  )
}

// A search's pages: the first, and those "more" loads one at a time, also for two clicks in
// the same tick (as the history's Older).
function usePages<P extends { next_offset?: number }>(url: string | null) {
  const first = useLoad<P | undefined>(() => (url ? api<P>(url) : Promise.resolve(undefined)), [url])
  const [more, setMore] = useState<P[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const pages = first.data ? [first.data, ...more] : []
  // Answered, or skipped (no url): what it found is all there is of it. A failed search is not
  // answered, and is not "nothing found" either.
  const answered = url === null || first.data !== undefined
  // Answered or failed: nothing more is awaited of it.
  const settled = answered || first.error !== null
  const last = pages.at(-1)
  const loadingMore = useRef(false)
  const loadMore = async () => {
    if (url === null || last?.next_offset === undefined || loadingMore.current) return
    loadingMore.current = true
    setLoading(true)
    setError(null)
    try {
      const page = await api<P>(`${url}&offset=${last.next_offset}`)
      setMore((m) => [...m, page])
    } catch (e) {
      setError(errorText(e))
    } finally {
      loadingMore.current = false
      setLoading(false)
    }
  }
  return { first, pages, last, loading, error, loadMore, answered, settled }
}

function ResultPages({
  workspace,
  url,
  cardsUrl,
  recordsUrl,
  conversationsUrl,
  query,
  onOpen,
  onClose,
}: {
  workspace: number
  url: string
  cardsUrl: string
  recordsUrl: string
  conversationsUrl: string | null
  query: SearchQuery
  onOpen: (result: SearchResult) => void
  onClose: () => void
}) {
  const docs = usePages<SearchPage>(url)
  const cards = usePages<CardPage>(cardsUrl)
  const records = usePages<RecordPage>(recordsUrl)
  const conversations = usePages<ConversationPage>(conversationsUrl)
  const { first, pages, last, loading, error } = docs
  const results = pages.flatMap((p) => p.results)
  const cardResults = cards.pages.flatMap((p) => p.results)
  const recordResults = records.pages.flatMap((p) => p.results)
  const conversationResults = conversations.pages.flatMap((p) => p.results)
  const t = useT()
  const format = useFormat()
  // The notice of the latest page: the index may have caught up meanwhile.
  const notice = last && noticeText(t, last)
  // The cards, the records and the conversations are searches of their own: one that fails or
  // is slow leaves the documents as they are, and says so beside them. Nothing is "not found"
  // until every search answered (one that failed hasn't).
  const othersAnswered = cards.answered && records.answered && conversations.answered
  // A records page with none to show may be followed by pages that have some.
  const recordsMore = records.last?.next_offset !== undefined
  const nothingElse =
    cardResults.length === 0 && recordResults.length === 0 && conversationResults.length === 0 && !recordsMore

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
      {first.data && results.length === 0 && othersAnswered && nothingElse && (
        <p className="tt-muted">{t('search.results.none')}</p>
      )}
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
                <Snippet pieces={r.snippet} />
              </button>
            </li>
          ))}
        </ul>
      )}
      {last?.next_offset !== undefined && (
        <button type="button" className="tt-button" onClick={docs.loadMore} disabled={loading}>
          {t('search.results.more')}
        </button>
      )}
      {(first.data || first.error) && !cards.settled && <p className="tt-muted">{t('search.cards.searching')}</p>}
      {cards.first.error && (
        <p role="alert" className="tt-error">
          {t('search.cards.failed', { error: cards.first.error })}
        </p>
      )}
      {cards.pages.some((p) => p.incomplete) && (
        <p role="status" className="tt-notice">
          {t('search.cards.incomplete')}
        </p>
      )}
      {cardResults.length > 0 && (
        <>
          <h3>{t('search.cards.title')}</h3>
          <ul className="tt-list" aria-label={t('search.cards.label')}>
            {cardResults.map((c) => (
              <li key={c.card_id}>
                <Link
                  href={routePath({
                    workspace,
                    section: 'boards',
                    item: boardItem(c.space_id, c.board_id, c.card_id),
                  })}
                >
                  <span className="tt-list-title">{c.title}</span>
                  <span className="tt-muted tt-small tt-list-title">
                    {t('search.cards.in', { space: c.space_name, board: c.board_name, column: t(COLUMNS[c.column]) })}
                  </span>
                  <Snippet pieces={c.snippet} />
                </Link>
              </li>
            ))}
          </ul>
          {cards.last?.next_offset !== undefined && (
            <button type="button" className="tt-button" onClick={cards.loadMore} disabled={cards.loading}>
              {t('search.cards.more')}
            </button>
          )}
          {cards.error && (
            <p role="alert" className="tt-error">
              {cards.error}
            </p>
          )}
        </>
      )}
      {(first.data || first.error) && !records.settled && <p className="tt-muted">{t('search.records.searching')}</p>}
      {records.first.error && (
        <p role="alert" className="tt-error">
          {t('search.records.failed', { error: records.first.error })}
        </p>
      )}
      {records.pages.some((p) => p.incomplete) && (
        <p role="status" className="tt-notice">
          {t('search.records.incomplete')}
        </p>
      )}
      {recordResults.length > 0 && (
        <>
          <h3>{t('search.records.title')}</h3>
          <ul className="tt-list" aria-label={t('search.records.label')}>
            {recordResults.map((r) => (
              <li key={`${r.app_id}/${r.record_id}`}>
                <Link href={appPath(workspace, r.space_id, r.app_id)}>
                  <span className="tt-list-title">{r.app_name}</span>
                  <span className="tt-muted tt-small tt-list-title">
                    {t('search.records.in', { space: r.space_name, app: r.app_name })}
                  </span>
                  <Snippet pieces={r.snippet} />
                </Link>
              </li>
            ))}
          </ul>
        </>
      )}
      {/* Also when this page held none the caller reads: the later ones may. */}
      {recordsMore && (
        <button type="button" className="tt-button" onClick={records.loadMore} disabled={records.loading}>
          {t('search.records.more')}
        </button>
      )}
      {records.error && (
        <p role="alert" className="tt-error">
          {records.error}
        </p>
      )}
      {(first.data || first.error) && !conversations.settled && (
        <p className="tt-muted">{t('search.conversations.searching')}</p>
      )}
      {conversations.first.error && (
        <p role="alert" className="tt-error">
          {t('search.conversations.failed', { error: conversations.first.error })}
        </p>
      )}
      {conversations.pages.some((p) => p.incomplete) && (
        <p role="status" className="tt-notice">
          {t('search.conversations.incomplete')}
        </p>
      )}
      {conversationResults.length > 0 && (
        <>
          <h3>{t('search.conversations.title')}</h3>
          <ul className="tt-list" aria-label={t('search.conversations.label')}>
            {conversationResults.map((c) => (
              <li key={c.conversation_id}>
                <Link href={routePath({ workspace, section: 'conversations', item: String(c.conversation_id) })}>
                  <span className="tt-list-title">{c.title || t('search.conversations.untitled')}</span>
                  <span className="tt-muted tt-small tt-list-title">
                    {t(c.archived ? 'search.conversations.archivedAt' : 'search.conversations.updated', {
                      when: format.dateTime(c.updated_at),
                    })}
                  </span>
                  <Snippet pieces={c.snippet} />
                </Link>
              </li>
            ))}
          </ul>
          {conversations.last?.next_offset !== undefined && (
            <button
              type="button"
              className="tt-button"
              onClick={conversations.loadMore}
              disabled={conversations.loading}
            >
              {t('search.conversations.more')}
            </button>
          )}
          {conversations.error && (
            <p role="alert" className="tt-error">
              {conversations.error}
            </p>
          )}
        </>
      )}
    </section>
  )
}

// A snippet: plain text in pieces, never markup: matches are marked by the element.
function Snippet({ pieces }: { pieces: { text: string; match?: boolean }[] }) {
  if (pieces.length === 0) return null
  return (
    <span className="tt-snippet tt-small">
      {pieces.map((s, i) =>
        // biome-ignore lint/suspicious/noArrayIndexKey: pieces have no identity but their place
        s.match ? <mark key={i}>{s.text}</mark> : <span key={i}>{s.text}</span>,
      )}
    </span>
  )
}

const COLUMNS: Record<CardResult['column'], Key> = {
  backlog: 'boards.columns.backlog',
  todo: 'boards.columns.todo',
  doing: 'boards.columns.doing',
  review: 'boards.columns.review',
  done: 'boards.columns.done',
  archived: 'boards.columns.archived',
}
