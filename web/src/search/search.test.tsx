// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { KnowledgePage } from '../knowledge/KnowledgePage'
import { fakeFetch } from '../test/fakes'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const spaces = [
  { id: 2, workspace_id: 1, kind: 'private', name: 'Private', archived_at: null, role: 'owner', access: 'write' },
  { id: 3, workspace_id: 1, kind: 'shared', name: 'Team', archived_at: null, role: 'viewer', access: 'read' },
]

const result = (space: (typeof spaces)[number], path: string, title: string) => ({
  space_id: space.id,
  space_name: space.name,
  path,
  title,
  snippet: [{ text: '…we grow the ' }, { text: 'orchard', match: true }, { text: ' <b>next</b> year…' }],
  score: 0.03,
  match: 'both',
  version: 'a'.repeat(40),
  commit: 'c'.repeat(40),
})

function search(q: string, scope?: 'In all spaces') {
  fireEvent.change(screen.getByLabelText('Search the knowledge base'), { target: { value: q } })
  if (scope) fireEvent.change(screen.getByLabelText('Where to search'), { target: { value: 'workspace' } })
  fireEvent.click(screen.getByRole('button', { name: 'Search' }))
}

test('searches the space shown, and shows matches as marked text, never markup', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/spaces/2/search': {
      body: { results: [result(spaces[0], 'notes/plan.md', 'Quarterly plan')], mode: 'hybrid' },
    },
    'GET /api/workspaces/1/spaces/2/search/cards': { body: { results: [] } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('  orchard plan ')
  const results = await screen.findByRole('region', { name: 'Search results' })
  expect(calls.filter((c) => c.path.includes('/search')).map((c) => c.path)).toEqual([
    '/api/workspaces/1/spaces/2/search?q=orchard%20plan',
    '/api/workspaces/1/spaces/2/search/cards?q=orchard%20plan',
  ])
  const hit = await within(results).findByRole('button', { name: /Quarterly plan/ })
  expect(within(hit).getByText('orchard').tagName).toBe('MARK')
  expect(hit.textContent).toContain('<b>next</b>')
  expect(hit.querySelector('b')).toBeNull()
  // A hybrid search with nothing to report shows no notice.
  expect(within(results).queryByRole('status')).toBeNull()

  fireEvent.click(within(results).getByRole('button', { name: 'Close' }))
  expect(screen.queryByRole('region', { name: 'Search results' })).toBeNull()
  expect(screen.getByText('No documents in this space yet.')).toBeTruthy()
})

test.each([
  'Semantic search is off: no embeddings model is set for this workspace. These are full-text results.',
  "Semantic search is off for these spaces until one of their members turns it on for this workspace's embeddings model. These are full-text results.",
  'The index is catching up with recent changes: some results may be missing or out of date.',
  '2 documents are still being embedded; until they are, only their text is searched.',
])('shows the server’s notice: %s', async (notice) => {
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/search': { body: { results: [], mode: 'fulltext', notice } },
    'GET /api/workspaces/1/search/cards': { body: { results: [] } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard', 'In all spaces')
  const results = await screen.findByRole('region', { name: 'Search results' })
  expect((await within(results).findByRole('status')).textContent).toBe(notice)
  expect(within(results).getByText('Nothing found.')).toBeTruthy()
})

test('says the notice from its codes, with the count in the plural it needs', async () => {
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/search': {
      body: {
        results: [],
        mode: 'hybrid',
        notice: 'the server’s words',
        notices: [{ code: 'behind' }, { code: 'pending', count: 1 }],
      },
    },
    'GET /api/workspaces/1/search/cards': { body: { results: [] } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard', 'In all spaces')
  const results = await screen.findByRole('region', { name: 'Search results' })
  expect((await within(results).findByRole('status')).textContent).toBe(
    'The index is catching up with recent changes: some results may be missing or out of date. ' +
      '1 document is still being embedded; until it is, only its text is searched.',
  )
})

test('searches every space, pages on, and opens a result in its own space', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/spaces/3/docs': { body: { docs: [{ path: 'team.md', version: 'a'.repeat(40), size: 4 }] } },
    'GET /api/workspaces/1/spaces/3/docs/team.md': {
      body: { path: 'team.md', version: 'a'.repeat(40), size: 4, content: 'Team' },
    },
    'GET /api/workspaces/1/search?q=orchard': {
      body: {
        results: [result(spaces[0], 'mine.md', 'Mine')],
        mode: 'fulltext',
        notice: '1 documents are still being embedded; until they are, only their text is searched.',
        next_offset: 1,
      },
    },
    'GET /api/workspaces/1/search?q=orchard&offset=1': {
      body: { results: [result(spaces[1], 'team.md', 'Team notes')], mode: 'hybrid' },
    },
    'GET /api/workspaces/1/search/cards': { body: { results: [] } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard', 'In all spaces')
  const results = await screen.findByRole('region', { name: 'Search results' })
  await within(results).findByRole('button', { name: /Mine/ })
  expect(within(results).getByRole('status')).toBeTruthy()

  fireEvent.click(within(results).getByRole('button', { name: 'More results' }))
  const team = await within(results).findByRole('button', { name: /Team notes/ })
  expect(team.textContent).toContain('Team · team.md')
  // The latest page's notice: it is no longer embedding.
  expect(within(results).queryByRole('status')).toBeNull()
  expect(within(results).queryByRole('button', { name: 'More results' })).toBeNull()

  fireEvent.click(team)
  const doc = await screen.findByRole('article', { name: 'team.md' })
  expect(await within(doc).findByText('Team')).toBeTruthy()
  expect((screen.getByLabelText('Space') as HTMLSelectElement).value).toBe('3')
  expect(calls.some((c) => c.path === '/api/workspaces/1/spaces/3/docs/team.md')).toBe(true)
  // Team is read-only for this user.
  expect(within(doc).queryByRole('button', { name: 'Edit' })).toBeNull()
})

test('searching the same again asks the server again', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/spaces/2/search': { body: { results: [], mode: 'fulltext' } },
    'GET /api/workspaces/1/spaces/2/search/cards': { body: { results: [] } },
  })
  const searches = () => calls.filter((c) => c.path.includes('/search?')).length
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard')
  await screen.findByText('Nothing found.')
  search('orchard')
  await waitFor(() => expect(searches()).toBe(2))
})

const card = (id: number, title: string, column = 'doing', board = { id: 7, name: 'Roadmap' }) => ({
  space_id: 3,
  space_name: 'Team',
  board_id: board.id,
  board_name: board.name,
  card_id: id,
  title,
  column,
  snippet: [{ text: 'ship the ' }, { text: 'orchard', match: true }, { text: ' <i>release</i>' }],
})

test('shows cards apart from documents, each linking to its card', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/search': {
      body: { results: [result(spaces[1], 'team.md', 'Team notes')], mode: 'fulltext' },
    },
    'GET /api/workspaces/1/search/cards?q=orchard': {
      body: { results: [card(11, 'Orchard rollout'), card(12, 'Orchard audit', 'archived')], next_offset: 2 },
    },
    'GET /api/workspaces/1/search/cards?q=orchard&offset=2': { body: { results: [card(13, 'Orchard party', 'done')] } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard', 'In all spaces')
  const results = await screen.findByRole('region', { name: 'Search results' })
  await within(results).findByRole('button', { name: /Team notes/ })
  const list = await within(results).findByRole('list', { name: 'Cards found' })
  expect(within(results).getByRole('heading', { name: 'Cards', level: 3 })).toBeTruthy()
  const links = within(list).getAllByRole('link')
  expect(links.map((a) => a.getAttribute('href'))).toEqual(['/w/1/boards/3%3A7%3A11', '/w/1/boards/3%3A7%3A12'])
  expect(links[0].textContent).toContain('Team · Roadmap · Doing')
  expect(links[1].textContent).toContain('Team · Roadmap · Archived')
  // Matches are marked by the element, never as markup.
  expect(within(links[0]).getByText('orchard').tagName).toBe('MARK')
  expect(links[0].textContent).toContain('<i>release</i>')
  expect(links[0].querySelector('i')).toBeNull()

  fireEvent.click(within(results).getByRole('button', { name: 'More cards' }))
  await within(list).findByRole('link', { name: /Orchard party/ })
  expect(within(results).queryByRole('button', { name: 'More cards' })).toBeNull()
  expect(calls.some((c) => c.path === '/api/workspaces/1/search/cards?q=orchard&offset=2')).toBe(true)
  // Never a card in the documents' list.
  expect(within(results).queryByText('Nothing found.')).toBeNull()
})

test('a search that finds only cards is not nothing', async () => {
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/spaces/2/search': { body: { results: [], mode: 'fulltext' } },
    'GET /api/workspaces/1/spaces/2/search/cards': { body: { results: [card(11, 'Orchard rollout')] } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard')
  const results = await screen.findByRole('region', { name: 'Search results' })
  await within(results).findByRole('link', { name: /Orchard rollout/ })
  expect(within(results).queryByText('Nothing found.')).toBeNull()
})

test('cards that cannot be searched leave the documents, and say so', async () => {
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/spaces/2/search': {
      body: { results: [result(spaces[0], 'notes/plan.md', 'Quarterly plan')], mode: 'fulltext' },
    },
    'GET /api/workspaces/1/spaces/2/search/cards': { status: 500, body: { error: 'boom', code: 'internal' } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard')
  const results = await screen.findByRole('region', { name: 'Search results' })
  await within(results).findByRole('button', { name: /Quarterly plan/ })
  expect((await within(results).findByRole('alert')).textContent).toContain('The boards’ cards could not be searched')
})

test('a failed card search is not "nothing found" when the documents have none', async () => {
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/spaces/2/search': { body: { results: [], mode: 'fulltext' } },
    'GET /api/workspaces/1/spaces/2/search/cards': { status: 500, body: { error: 'boom', code: 'internal' } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard')
  const results = await screen.findByRole('region', { name: 'Search results' })
  expect((await within(results).findByRole('alert')).textContent).toContain('The boards’ cards could not be searched')
  expect(within(results).queryByText('Nothing found.')).toBeNull()
})

test('says when cards that changed just now were left out', async () => {
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/spaces/2/search': { body: { results: [], mode: 'fulltext' } },
    'GET /api/workspaces/1/spaces/2/search/cards': { body: { results: [], incomplete: true } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard')
  const results = await screen.findByRole('region', { name: 'Search results' })
  expect((await within(results).findByRole('status')).textContent).toContain('changed just now')
})
