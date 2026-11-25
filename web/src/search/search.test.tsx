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

// Records and conversations are searched too: unless a test says otherwise, they find nothing.
const none = { body: { results: [] } }
function fakeSearch(routes: Parameters<typeof fakeFetch>[0]) {
  return fakeFetch({
    'GET /api/workspaces/1/search/records': none,
    'GET /api/workspaces/1/spaces/2/search/records': none,
    'GET /api/workspaces/1/spaces/3/search/records': none,
    'GET /api/workspaces/1/search/conversations': none,
    ...routes,
  })
}

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
  const calls = fakeSearch({
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
    '/api/workspaces/1/spaces/2/search/records?q=orchard%20plan',
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
  fakeSearch({
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
  fakeSearch({
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
  const calls = fakeSearch({
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
  const calls = fakeSearch({
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

test("a result's snippet shows without the document's Markdown marks, its matches still marked", async () => {
  const hit = {
    ...result(spaces[0], 'notes/plan.md', 'Quarterly plan'),
    snippet: [{ text: '## Plan\n\nThe **first ' }, { text: 'orchard', match: true }, { text: '** is `small`' }],
  }
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/spaces/2/search': { body: { results: [hit], mode: 'hybrid' } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard')
  const results = await screen.findByRole('region', { name: 'Search results' })
  const shown = await within(results).findByRole('button', { name: /Quarterly plan/ })
  const snippet = shown.querySelector('.tt-snippet')
  expect(snippet?.textContent).toBe('Plan The first orchard is small')
  expect([...(snippet?.querySelectorAll('mark') ?? [])].map((m) => m.textContent)).toEqual(['orchard'])
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
  const calls = fakeSearch({
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
  fakeSearch({
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
  fakeSearch({
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
  fakeSearch({
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
  fakeSearch({
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

test('says the cards are still being searched while the documents are shown', async () => {
  let release: () => void = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  fakeSearch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/spaces/2/search': {
      body: { results: [result(spaces[0], 'notes/plan.md', 'Quarterly plan')], mode: 'fulltext' },
    },
    'GET /api/workspaces/1/spaces/2/search/cards': async () => {
      await held
      return { body: { results: [card(11, 'Orchard rollout')] } }
    },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard')
  const results = await screen.findByRole('region', { name: 'Search results' })
  await within(results).findByRole('button', { name: /Quarterly plan/ })
  expect(within(results).getByText('Searching the boards’ cards…')).toBeTruthy()
  release()
  await within(results).findByRole('link', { name: /Orchard rollout/ })
  expect(within(results).queryByText('Searching the boards’ cards…')).toBeNull()
})

const record = (id: number, app: string, snippet = 'lunch at the orchard') => ({
  space_id: 3,
  space_name: 'Team',
  app_id: 9,
  app_name: app,
  record_id: id,
  snippet: [{ text: `${snippet.split('orchard')[0]}` }, { text: 'orchard', match: true }, { text: ' <u>cafe</u>' }],
})

const conversation = (id: number, title: string, archived = false) => ({
  conversation_id: id,
  title,
  archived,
  updated_at: '2026-11-24T18:00:00.000Z',
  snippet: [{ text: 'we planned the ' }, { text: 'orchard', match: true }, { text: ' <b>visit</b>' }],
})

test('shows records and conversations apart from documents, each linking to its place', async () => {
  const calls = fakeSearch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/search': { body: { results: [], mode: 'fulltext' } },
    'GET /api/workspaces/1/search/cards': none,
    'GET /api/workspaces/1/search/records?q=orchard': {
      body: { results: [record(5, 'Expenses'), record(6, 'Expenses')], next_offset: 2 },
    },
    'GET /api/workspaces/1/search/records?q=orchard&offset=2': { body: { results: [record(7, 'Garden log')] } },
    'GET /api/workspaces/1/search/conversations?q=orchard': {
      body: { results: [conversation(21, 'Orchard trip'), conversation(22, '', true)], next_offset: 2 },
    },
    'GET /api/workspaces/1/search/conversations?q=orchard&offset=2': { body: { results: [conversation(23, 'More')] } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard', 'In all spaces')
  const results = await screen.findByRole('region', { name: 'Search results' })

  const records = await within(results).findByRole('list', { name: 'Records found' })
  expect(within(results).getByRole('heading', { name: 'Records', level: 3 })).toBeTruthy()
  const recordLinks = within(records).getAllByRole('link')
  expect(recordLinks.map((a) => a.getAttribute('href'))).toEqual(['/w/1/apps/3-9', '/w/1/apps/3-9'])
  expect(recordLinks[0].textContent).toContain('Team · Expenses')
  expect(within(recordLinks[0]).getByText('orchard').tagName).toBe('MARK')
  expect(recordLinks[0].textContent).toContain('<u>cafe</u>')
  expect(recordLinks[0].querySelector('u')).toBeNull()
  fireEvent.click(within(results).getByRole('button', { name: 'More records' }))
  await within(records).findByRole('link', { name: /Garden log/ })
  expect(within(results).queryByRole('button', { name: 'More records' })).toBeNull()

  const list = await within(results).findByRole('list', { name: 'Conversations found' })
  const links = within(list).getAllByRole('link')
  expect(links.map((a) => a.getAttribute('href'))).toEqual(['/w/1/conversations/21', '/w/1/conversations/22'])
  expect(links[0].textContent).toContain('Orchard trip')
  expect(links[0].textContent).toContain('Updated')
  expect(links[1].textContent).toContain('Untitled conversation')
  expect(links[1].textContent).toContain('Archived')
  expect(within(links[0]).getByText('orchard').tagName).toBe('MARK')
  expect(links[0].querySelector('b')).toBeNull()
  fireEvent.click(within(results).getByRole('button', { name: 'More conversations' }))
  await within(list).findByRole('link', { name: /More/ })
  expect(calls.some((c) => c.path === '/api/workspaces/1/search/conversations?q=orchard&offset=2')).toBe(true)
  // Something was found: not "nothing".
  expect(within(results).queryByText('Nothing found.')).toBeNull()
})

test('a search of one space leaves conversations out: they are not in a space', async () => {
  const calls = fakeSearch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/spaces/2/search': { body: { results: [], mode: 'fulltext' } },
    'GET /api/workspaces/1/spaces/2/search/cards': none,
    'GET /api/workspaces/1/spaces/2/search/records': { body: { results: [record(5, 'Expenses')] } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard')
  const results = await screen.findByRole('region', { name: 'Search results' })
  await within(results).findByRole('list', { name: 'Records found' })
  expect(calls.some((c) => c.path.includes('/search/conversations'))).toBe(false)
  expect(within(results).queryByRole('list', { name: 'Conversations found' })).toBeNull()
})

test('records or conversations that cannot be searched leave the rest, and say so', async () => {
  fakeSearch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/search': {
      body: { results: [result(spaces[1], 'team.md', 'Team notes')], mode: 'fulltext' },
    },
    'GET /api/workspaces/1/search/cards': none,
    'GET /api/workspaces/1/search/records': { status: 500, body: { error: 'boom', code: 'internal' } },
    'GET /api/workspaces/1/search/conversations': { status: 500, body: { error: 'boom', code: 'internal' } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard', 'In all spaces')
  const results = await screen.findByRole('region', { name: 'Search results' })
  await within(results).findByRole('button', { name: /Team notes/ })
  const alerts = await within(results).findAllByRole('alert')
  expect(alerts.map((a) => a.textContent).join('|')).toContain('The apps’ records could not be searched')
  await waitFor(() =>
    expect(
      within(results)
        .getAllByRole('alert')
        .map((a) => a.textContent)
        .join('|'),
    ).toContain('Your conversations could not be searched'),
  )
})

test('says when records or conversations that changed just now were left out', async () => {
  fakeSearch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/search': { body: { results: [], mode: 'fulltext' } },
    'GET /api/workspaces/1/search/cards': none,
    'GET /api/workspaces/1/search/records': { body: { results: [], incomplete: true } },
    'GET /api/workspaces/1/search/conversations': { body: { results: [], incomplete: true } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard', 'In all spaces')
  const results = await screen.findByRole('region', { name: 'Search results' })
  expect(await within(results).findByText(/Some records changed just now/)).toBeTruthy()
  expect(await within(results).findByText(/Some conversations changed just now/)).toBeTruthy()
})

test('a search that finds only records or conversations is not nothing', async () => {
  fakeSearch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/search': { body: { results: [], mode: 'fulltext' } },
    'GET /api/workspaces/1/search/cards': none,
    'GET /api/workspaces/1/search/conversations': { body: { results: [conversation(21, 'Orchard trip')] } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard', 'In all spaces')
  const results = await screen.findByRole('region', { name: 'Search results' })
  await within(results).findByRole('link', { name: /Orchard trip/ })
  expect(within(results).queryByText('Nothing found.')).toBeNull()
})

test('a page of records that has none to show still leads to the next', async () => {
  fakeSearch({
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/spaces/2/docs': { body: { docs: [] } },
    'GET /api/workspaces/1/search': { body: { results: [], mode: 'fulltext' } },
    'GET /api/workspaces/1/search/cards': none,
    'GET /api/workspaces/1/search/records?q=orchard': { body: { results: [], next_offset: 1 } },
    'GET /api/workspaces/1/search/records?q=orchard&offset=1': { body: { results: [record(7, 'Garden log')] } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard', 'In all spaces')
  const results = await screen.findByRole('region', { name: 'Search results' })
  const more = await within(results).findByRole('button', { name: 'More records' })
  // Not "nothing found" while a page of records is still to come.
  expect(within(results).queryByText('Nothing found.')).toBeNull()
  fireEvent.click(more)
  await within(results).findByRole('link', { name: /Garden log/ })
})
