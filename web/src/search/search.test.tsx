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
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('  orchard plan ')
  const results = await screen.findByRole('region', { name: 'Search results' })
  expect(calls.at(-1)?.path).toBe('/api/workspaces/1/spaces/2/search?q=orchard%20plan')
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
  })
  const searches = () => calls.filter((c) => c.path.includes('/search')).length
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  search('orchard')
  await screen.findByText('Nothing found.')
  search('orchard')
  await waitFor(() => expect(searches()).toBe(2))
})
