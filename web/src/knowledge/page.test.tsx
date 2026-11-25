// @vitest-environment jsdom

// The knowledge base's page as it is laid out: the tree of folders, where the keyboard goes when
// a part of the page changes, the one primary action, loading, empty and failing states.

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { fakeFetch } from '../test/fakes'
import { buildTree } from './DocList'
import { KnowledgePage } from './KnowledgePage'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const S = '/api/workspaces/1/spaces/3'
const V1 = 'a'.repeat(40)
const V2 = 'b'.repeat(40)
const space = (access: 'write' | 'read') => ({
  id: 3,
  workspace_id: 1,
  kind: 'shared',
  name: 'Team',
  archived_at: null,
  role: access === 'write' ? 'editor' : 'viewer',
  access,
})
const doc = (path: string, size = 7) => ({ path, version: V1, size, versioned: true })
const docs = [doc('notes/plan.md'), doc('notes/2026/q1.md'), doc('ideas.md'), doc('archive/old.md')]

function kb(access: 'write' | 'read', extra: Parameters<typeof fakeFetch>[0] = {}) {
  return fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces: [space(access)] } },
    [`GET ${S}/docs`]: { body: { docs } },
    [`GET ${S}/docs/notes/plan.md`]: { body: { ...doc('notes/plan.md'), content: '# Plan\n\nThe draft.\n' } },
    [`GET ${S}/docs/ideas.md`]: { body: { ...doc('ideas.md'), content: '# Ideas\n' } },
    ...extra,
  })
}

const folder = (name: string) => screen.getByRole('button', { name: new RegExp(`^${name}`) })
const heading = (article: HTMLElement) => within(article).getByRole('heading', { level: 2 })

test('the tree: folders from the paths, first and in order; a closed one says how many it holds', async () => {
  const tree = buildTree(docs)
  expect(tree.folders.map((f) => f.name)).toEqual(['archive', 'notes'])
  expect(tree.files.map((f) => f.path)).toEqual(['ideas.md'])
  expect(tree.folders[1].folders[0].path).toBe('notes/2026')
  expect(tree.folders[1].count).toBe(2)

  kb('read')
  render(<KnowledgePage workspace={1} />)
  const list = await screen.findByRole('list', { name: 'Documents' })
  const notes = within(list).getByRole('button', { name: /^notes/ })
  expect(notes.getAttribute('aria-expanded')).toBe('false')
  expect(notes.textContent).toContain('2 documents')
  expect(within(list).queryByRole('button', { name: /^plan\.md/ })).toBeNull()

  fireEvent.click(notes)
  expect(notes.getAttribute('aria-expanded')).toBe('true')
  expect(notes.textContent).not.toContain('2 documents')
  // Inside it: its folder first, then its document.
  const inside = [...(notes.nextElementSibling?.querySelectorAll(':scope > li > button') ?? [])].map(
    (b) => b.textContent,
  )
  expect(inside[0]).toMatch(/^2026/)
  expect(inside[1]).toMatch(/^plan\.md/)

  fireEvent.click(within(list).getByRole('button', { name: /^plan\.md/ }))
  await screen.findByRole('article', { name: 'notes/plan.md' })
  expect(
    within(list)
      .getByRole('button', { name: /^plan\.md/ })
      .getAttribute('aria-current'),
  ).toBe('true')
  fireEvent.click(notes)
  expect(within(list).queryByRole('button', { name: /^plan\.md/ })).toBeNull()
})

test('a document opened by a link shows where it is in the tree', async () => {
  kb('read')
  render(<KnowledgePage workspace={1} open="3:notes/plan.md" />)
  const article = await screen.findByRole('article', { name: 'notes/plan.md' })
  await screen.findByRole('list', { name: 'Documents' })
  expect(folder('notes').getAttribute('aria-expanded')).toBe('true')
  expect(screen.getByRole('button', { name: /^plan\.md/ }).getAttribute('aria-current')).toBe('true')
  // The path line: the space, the folders, the name, and the size.
  expect(heading(article).textContent).toBe('Team/notes/plan.md')
  expect(within(article).getByText('7 B')).toBeTruthy()
  // A page that opens on a document leaves the keyboard alone, also once the document has come.
  await within(article).findByRole('heading', { name: 'Plan' })
  expect(document.activeElement).toBe(document.body)
})

test('back from a document gives the keyboard to it in the tree', async () => {
  kb('read')
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: /^ideas\.md/ }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  const back = within(article).getByRole('button', { name: 'Back to the documents' })
  back.focus()
  fireEvent.click(back)
  expect(screen.queryByRole('article')).toBeNull()
  expect(document.activeElement).toBe(screen.getByRole('button', { name: /^ideas\.md/ }))
})

test('saving or cancelling an edit gives the keyboard back to Edit', async () => {
  kb('write', { [`PUT ${S}/docs/ideas.md`]: { body: { commit: 'c2', doc: { ...doc('ideas.md'), version: V2 } } } })
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: /^ideas\.md/ }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  for (const leave of ['Cancel', 'Save']) {
    fireEvent.click(await within(article).findByRole('button', { name: 'Edit' }))
    const text = within(article).getByLabelText('Text')
    expect(document.activeElement).toBe(text)
    fireEvent.click(within(article).getByRole('button', { name: leave }))
    await waitFor(() => expect(within(article).queryByLabelText('Text')).toBeNull())
    await waitFor(() => expect(document.activeElement).toBe(within(article).getByRole('button', { name: 'Edit' })))
  }
})

test('one primary action on the screen, and a document heading with at most three', async () => {
  kb('write')
  render(<KnowledgePage workspace={1} />)
  const primary = () => [...document.querySelectorAll('.ui-button-primary')].map((b) => b.textContent)
  await screen.findByRole('list', { name: 'Documents' })
  expect(primary()).toEqual(['New'])

  fireEvent.click(screen.getByRole('button', { name: /^ideas\.md/ }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  await within(article).findByRole('button', { name: 'Edit' })
  expect(primary()).toEqual(['New'])
  // Edit, History, and the rest in a menu.
  const bar = heading(article).closest('.kb-doc-bar') as HTMLElement
  expect(
    within(bar)
      .getAllByRole('button')
      .map((b) => b.getAttribute('aria-label') ?? b.textContent),
  ).toEqual(['Back to the documents', 'Edit', 'History', 'More actions'])
  fireEvent.click(within(bar).getByRole('button', { name: 'More actions' }))
  expect(
    within(bar)
      .getAllByRole('menuitem')
      .map((m) => m.textContent),
  ).toEqual(['Move', 'Delete'])
  fireEvent.keyDown(within(bar).getByRole('menu'), { key: 'Escape' })

  // Editing: Save is the one primary, and New steps back.
  fireEvent.click(within(article).getByRole('button', { name: 'Edit' }))
  await waitFor(() => expect(primary()).toEqual(['Save']))
})

test('the list loads as the shape of a list, and the document as the shape of a document', async () => {
  let release: () => void = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  kb('read', {
    [`GET ${S}/docs`]: async () => {
      await held
      return { body: { docs } }
    },
  })
  render(<KnowledgePage workspace={1} open="3:ideas.md" />)
  const loading = await screen.findAllByText('Loading…')
  // Said to a screen reader, inside the shapes that stand in for what comes.
  for (const said of loading) expect(said.closest('[aria-busy="true"]')).not.toBeNull()
  release()
  await screen.findByRole('list', { name: 'Documents' })
  await screen.findByRole('heading', { name: 'Ideas' })
  expect(screen.queryByText('Loading…')).toBeNull()
})

test('a list that fails says what failed, apart from the spaces, and tries again', async () => {
  let fail = true
  const calls = kb('read', {
    [`GET ${S}/docs`]: () => (fail ? { status: 500, body: { error: 'disk on fire' } } : { body: { docs } }),
  })
  render(<KnowledgePage workspace={1} />)
  const alert = await screen.findByRole('alert')
  expect(alert.textContent).toContain("The documents couldn't be loaded.")
  expect(alert.textContent).toContain('disk on fire')
  expect(screen.queryByText("The spaces couldn't be loaded.")).toBeNull()
  fail = false
  fireEvent.click(within(alert).getByRole('button', { name: 'Try again' }))
  await screen.findByRole('list', { name: 'Documents' })
  expect(calls.filter((c) => c.path.startsWith(`${S}/docs?`))).toHaveLength(2)
})

test('an empty space says so and offers to write or upload', async () => {
  kb('write', { [`GET ${S}/docs`]: { body: { docs: [] } } })
  render(<KnowledgePage workspace={1} />)
  await screen.findByText('No documents in this space yet.')
  // Its own New and Upload, beside the page's.
  expect(screen.getAllByRole('button', { name: 'New' })).toHaveLength(2)
  fireEvent.click(screen.getAllByRole('button', { name: 'Upload' }).at(-1) as HTMLElement)
  expect(screen.getByRole('dialog', { name: 'Upload a file' })).toBeTruthy()
  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(screen.queryByRole('dialog')).toBeNull()
})

test('a search result opens beside nothing: the document takes the keyboard, and back returns to the results', async () => {
  kb('read', {
    [`GET ${S}/search`]: {
      body: {
        results: [
          {
            space_id: 3,
            space_name: 'Team',
            path: 'ideas.md',
            title: 'Ideas',
            snippet: [{ text: 'some ' }, { text: 'ideas', match: true }],
            score: 1,
            match: 'text',
            version: V1,
            commit: 'c1',
          },
        ],
        mode: 'fulltext',
      },
    },
    [`GET ${S}/search/cards`]: { body: { results: [] } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByRole('list', { name: 'Documents' })
  fireEvent.change(screen.getByLabelText('Search the knowledge base'), { target: { value: 'ideas' } })
  fireEvent.click(screen.getByRole('button', { name: 'Search' }))
  const results = await screen.findByRole('region', { name: 'Search results' })
  fireEvent.click(await within(results).findByRole('button', { name: 'Ideas' }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  await waitFor(() => expect(document.activeElement).toBe(heading(article)))

  fireEvent.click(within(article).getByRole('button', { name: 'Back to the results' }))
  const again = await screen.findByRole('region', { name: 'Search results' })
  expect(document.activeElement).toBe(within(again).getByRole('heading', { level: 2 }))
})

test('a document is read in its own look, a column of numbers set to the end', async () => {
  kb('read', {
    [`GET ${S}/docs/ideas.md`]: {
      body: {
        ...doc('ideas.md'),
        content: '# Ideas\n\n| Plan | Seats |\n|---|---|\n| Solo | 1 |\n| Team | 1,000 |\n\n- [x] Done\n\n<b>raw</b>\n',
      },
    },
  })
  render(<KnowledgePage workspace={1} open="3:ideas.md" />)
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  const title = await within(article).findByRole('heading', { name: 'Ideas' })
  expect(title.className).toBe('ui-prose-heading')
  const [plan, seats] = within(article).getAllByRole('columnheader')
  expect(plan.dataset.numeric).toBeUndefined()
  expect(seats.dataset.numeric).toBe('true')
  expect(within(article).getByRole('cell', { name: '1,000' }).dataset.numeric).toBe('true')
  // A task is a box that can't be pressed; raw HTML stays text.
  expect((within(article).getByRole('checkbox', { name: 'Done' }) as HTMLInputElement).disabled).toBe(true)
  expect(within(article).getByText('<b>raw</b>')).toBeTruthy()
  expect(article.querySelector('b')).toBeNull()
})
