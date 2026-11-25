// Searching the knowledge base (docs/api.md, Search): a box that searches the space shown or
// every space of the workspace, and the results with the server's notice, which says when
// a search is full text only (no embeddings model, no consent, the model didn't answer), or
// what it can't find yet (documents still being embedded, an index catching up). The notice
// is said from its codes, in the user's language; the server's words are the fallback.
// The same words also search the cards of the boards, which come apart from the documents,
// under their own heading, each linking to its card.

import { type FormEvent, useRef, useState } from 'react'
import { api, errorText, type Space, ws } from '../api'
import { type Key, type T, useT } from '../i18n'
import { useLoad } from '../load'
import { boardItem, routePath } from '../router'
import { spaceLabel } from '../spaces'
import { Button } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { Heading } from '../ui/Heading'
import { Icon } from '../ui/Icon'
import { IconButton } from '../ui/IconButton'
import { Input } from '../ui/Input'
import { Notice as NoticeBox } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { Select } from '../ui/Select'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { cleanSnippet, isMarkdownPath } from './snippet'
import './search.css'

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
      <form className="search-box" onSubmit={submit}>
        <span className="search-field">
          <Input
            type="search"
            className="search-input"
            aria-label={t('search.box.label')}
            placeholder={t('search.box.placeholder')}
            value={q}
            maxLength={500}
            onChange={(e) => setQ(e.target.value)}
          />
          <IconButton type="submit" icon="search" size="sm" label={t('search.box.submit')} disabled={!q.trim()} />
        </span>
        {space && (
          <Select
            aria-label={t('search.box.where')}
            value={scope}
            onChange={(e) => setScope(e.target.value as SearchQuery['scope'])}
          >
            <option value="space">{t('search.box.inSpace', { space: spaceLabel(space) })}</option>
            <option value="workspace">{t('search.box.everywhere')}</option>
          </Select>
        )}
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
  return (
    <ResultPages
      key={url}
      workspace={workspace}
      url={url}
      cardsUrl={cardsUrl}
      onOpen={onOpen}
      onClose={onClose}
      query={query}
    />
  )
}

// A search's pages: the first, and those "more" loads one at a time, also for two clicks in
// the same tick (as the history's Older).
function usePages<P extends { next_offset?: number }>(url: string) {
  const first = useLoad(() => api<P>(url), [url])
  const [more, setMore] = useState<P[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const pages = first.data ? [first.data, ...more] : []
  const last = pages.at(-1)
  const loadingMore = useRef(false)
  const loadMore = async () => {
    if (last?.next_offset === undefined || loadingMore.current) return
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
  return { first, pages, last, loading, error, loadMore }
}

function ResultPages({
  workspace,
  url,
  cardsUrl,
  query,
  onOpen,
  onClose,
}: {
  workspace: number
  url: string
  cardsUrl: string
  query: SearchQuery
  onOpen: (result: SearchResult) => void
  onClose: () => void
}) {
  const docs = usePages<SearchPage>(url)
  const cards = usePages<CardPage>(cardsUrl)
  const { first, pages, last, loading, error } = docs
  const results = pages.flatMap((p) => p.results)
  const cardResults = cards.pages.flatMap((p) => p.results)
  const t = useT()
  // The notice of the latest page: the index may have caught up meanwhile.
  const notice = last && noticeText(t, last)
  // The cards are a search of their own: one that fails or is slow leaves the documents as
  // they are, and says so beside them. Nothing is "not found" until both searches answered.
  const cardsSettled = cards.first.data !== undefined

  return (
    <section className="search-results" aria-label={t('search.results.label')}>
      <div className="search-results-head">
        <Heading level={2} size="title" tabIndex={-1} data-results-title>
          {query.scope === 'workspace'
            ? t.rich('search.results.titleEverywhere', {
                q: query.q,
                muted: (text) => <span className="search-muted">{text}</span>,
              })
            : t('search.results.title', { q: query.q })}
        </Heading>
        <Button variant="ghost" icon="close" onClick={onClose}>
          {t('search.results.close')}
        </Button>
      </div>
      {notice && <NoticeBox tone="neutral">{notice}</NoticeBox>}
      {(first.error || error) && <NoticeBox tone="danger">{first.error ?? error}</NoticeBox>}
      {!first.data && !first.error && <Searching label={t('search.results.searching')} />}
      {first.data && results.length === 0 && cardsSettled && cardResults.length === 0 && (
        <EmptyState icon={<Icon name="search" size={24} />} title={t('search.results.none')} />
      )}
      {results.length > 0 && (
        <Panel>
          <Rows>
            {results.map((r) => (
              <Row
                key={`${r.space_id}/${r.path}`}
                leading={<Icon name="file" size={18} />}
                title={r.title || r.path}
                onOpen={() => onOpen(r)}
                detail={
                  <>
                    <span className="search-where">
                      {query.scope === 'workspace'
                        ? t('search.results.inSpace', { space: r.space_name, path: r.path })
                        : r.path}
                    </span>
                    <Snippet pieces={r.snippet} path={r.path} />
                  </>
                }
              />
            ))}
          </Rows>
        </Panel>
      )}
      {last?.next_offset !== undefined && (
        <div className="search-more">
          <Button size="sm" onClick={docs.loadMore} loading={loading}>
            {t('search.results.more')}
          </Button>
        </div>
      )}
      {(first.data || first.error) && !cards.first.data && !cards.first.error && (
        <Searching label={t('search.cards.searching')} />
      )}
      {cards.first.error && (
        <NoticeBox tone="danger">{t('search.cards.failed', { error: cards.first.error })}</NoticeBox>
      )}
      {cards.pages.some((p) => p.incomplete) && <NoticeBox tone="info">{t('search.cards.incomplete')}</NoticeBox>}
      {cardResults.length > 0 && (
        <>
          <Heading level={3}>{t('search.cards.title')}</Heading>
          <Panel>
            <Rows label={t('search.cards.label')}>
              {cardResults.map((c) => (
                <Row
                  key={c.card_id}
                  leading={<Icon name="boards" size={18} />}
                  title={c.title}
                  href={routePath({
                    workspace,
                    section: 'boards',
                    item: boardItem(c.space_id, c.board_id, c.card_id),
                  })}
                  detail={
                    <>
                      <span className="search-where">
                        {t('search.cards.in', {
                          space: c.space_name,
                          board: c.board_name,
                          column: t(COLUMNS[c.column]),
                        })}
                      </span>
                      <Snippet pieces={c.snippet} />
                    </>
                  }
                />
              ))}
            </Rows>
          </Panel>
          {cards.last?.next_offset !== undefined && (
            <div className="search-more">
              <Button size="sm" onClick={cards.loadMore} loading={cards.loading}>
                {t('search.cards.more')}
              </Button>
            </div>
          )}
          {cards.error && <NoticeBox tone="danger">{cards.error}</NoticeBox>}
        </>
      )}
    </section>
  )
}

// What a search shows while it waits: the shape of a few results, and its words for a screen reader.
function Searching({ label }: { label: string }) {
  return (
    <div className="search-loading" aria-busy="true">
      <VisuallyHidden>{label}</VisuallyHidden>
      {['45%', '35%', '50%'].map((width, i) => (
        <span key={width} className="search-loading-row">
          <Skeleton width={width} />
          <Skeleton width={i === 1 ? '80%' : '90%'} />
        </span>
      ))}
    </div>
  )
}

// A snippet: plain text in pieces, never markup: matches are marked by the element. A document's
// Markdown marks are left out for display (the server's result is unchanged); an upload's name
// and a card's words, which have none to show, are shown as they are (path is set for documents).
function Snippet({ pieces, path }: { pieces: SearchResult['snippet']; path?: string }) {
  const shown = cleanSnippet(pieces, { markdown: path !== undefined && isMarkdownPath(path) })
  if (shown.length === 0) return null
  return (
    <span className="search-snippet">
      {shown.map((s, i) =>
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
