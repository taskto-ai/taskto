// @vitest-environment jsdom

// The knowledge base's page as it is laid out: the tree of folders, where the keyboard goes when
// a part of the page changes, the one primary action, loading, empty and failing states.

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { fakeFetch } from '../test/fakes'
import { buildTree, type Folder } from './DocList'
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
  const inside = [...(notes.closest('li')?.querySelectorAll(':scope > ul > li') ?? [])].map(
    (li) => li.querySelector('button')?.textContent,
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

// Every document in a tree, as the paths its folders make, each once.
function paths(folder: Folder): string[] {
  return [
    ...folder.files.map((d) => d.path),
    ...folder.folders.flatMap((f) => {
      const inside = paths(f)
      expect(inside.every((p) => p.startsWith(`${f.path}/`))).toBe(true)
      return inside
    }),
  ]
}

test('the tree holds every document once, under the folders its path names', () => {
  const deep = Array.from({ length: 16 }, (_, i) => `d${i}`).join('/')
  const all = [
    'a',
    'a/b.md',
    'a/b/c.md',
    'x/y/z/w.md',
    `${deep}/bottom.md`,
    'with space/a name.md',
    'dots/v1.2.3.tar.gz',
    '日本語/メモ.md',
    'العربية/ملاحظة.md',
    'top.md',
  ].map((p) => doc(p))
  const tree = buildTree(all)
  expect(paths(tree).sort()).toEqual(all.map((d) => d.path).sort())
  expect(tree.count).toBe(all.length)
  // A document and a folder of the same name are both there.
  expect(tree.files.map((d) => d.path)).toContain('a')
  const a = tree.folders.find((f) => f.name === 'a') as Folder
  expect(a.files.map((d) => d.path)).toEqual(['a/b.md'])
  expect(a.folders.map((f) => f.path)).toEqual(['a/b'])
  expect(a.count).toBe(2)
  // Sixteen folders deep, the document is at the bottom of them.
  let at = tree
  for (let i = 0; i < 16; i++) at = at.folders.find((f) => f.name === `d${i}`) as Folder
  expect(at.files.map((d) => d.path)).toEqual([`${deep}/bottom.md`])
  // Each folder's count is every document under it.
  expect(tree.folders.find((f) => f.name === 'x')?.count).toBe(1)
  expect(tree.folders.find((f) => f.name === 'd0')?.count).toBe(1)
})

test('numbers sort as numbers', () => {
  const tree = buildTree(['q10.md', 'q2.md', 'q1.md'].map((p) => doc(p)))
  expect(tree.files.map((d) => d.path)).toEqual(['q1.md', 'q2.md', 'q10.md'])
})

test('a name the column cuts shows in full in a tooltip, and only then', async () => {
  const long = 'a-folder-name-that-is-much-too-long-for-the-column-of-the-tree'
  kb('read', { [`GET ${S}/docs`]: { body: { docs: [doc(`${long}/plan.md`), doc('ideas.md')] } } })
  // jsdom lays nothing out: a name longer than 30 characters is the one the column cuts.
  const sw = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'scrollWidth')
  const cw = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'clientWidth')
  Object.defineProperty(HTMLElement.prototype, 'scrollWidth', {
    configurable: true,
    get(this: HTMLElement) {
      return this.textContent?.length ?? 0
    },
  })
  Object.defineProperty(HTMLElement.prototype, 'clientWidth', {
    configurable: true,
    get() {
      return 30
    },
  })
  try {
    render(<KnowledgePage workspace={1} />)
    const list = await screen.findByRole('list', { name: 'Documents' })
    const tip = (name: RegExp) =>
      within(list).getByRole('button', { name }).closest('li')?.querySelector('[role="tooltip"]')?.textContent
    await waitFor(() => expect(tip(/^a-folder-name/)).toBe(long))
    expect(tip(/^ideas\.md/)).toBe('')
  } finally {
    if (sw) Object.defineProperty(HTMLElement.prototype, 'scrollWidth', sw)
    if (cw) Object.defineProperty(HTMLElement.prototype, 'clientWidth', cw)
  }
})

test('a folder opened in one space is not open in the next', async () => {
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces: [space('read'), { ...space('read'), id: 4, name: 'Other' }] } },
    [`GET ${S}/docs`]: { body: { docs } },
    'GET /api/workspaces/1/spaces/4/docs': { body: { docs } },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByRole('list', { name: 'Documents' })
  fireEvent.click(folder('notes'))
  expect(folder('notes').getAttribute('aria-expanded')).toBe('true')
  fireEvent.change(screen.getByLabelText('Space'), { target: { value: '4' } })
  await waitFor(() => expect(folder('notes').getAttribute('aria-expanded')).toBe('false'))
})

const ideasResult = {
  space_id: 3,
  space_name: 'Team',
  path: 'ideas.md',
  title: 'Ideas',
  snippet: [{ text: 'some ' }, { text: 'ideas', match: true }],
  score: 1,
  match: 'text',
  version: V1,
  commit: 'c1',
}
const searchRoutes = {
  [`GET ${S}/search`]: { body: { results: [ideasResult], mode: 'fulltext' } },
  [`GET ${S}/search/cards`]: { body: { results: [] } },
  [`GET ${S}/search/records`]: { body: { results: [] } },
}
function searchFor(q: string) {
  fireEvent.change(screen.getByLabelText('Search the knowledge base'), { target: { value: q } })
  fireEvent.click(screen.getByRole('button', { name: 'Search' }))
}

test('a search made while a document is open shows its results, and the document comes back', async () => {
  kb('read', searchRoutes)
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: /^ideas\.md/ }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  searchFor('ideas')
  const results = await screen.findByRole('region', { name: 'Search results' })
  await within(results).findByRole('button', { name: 'Ideas' })
  // The document is behind the results, not shown.
  expect(screen.queryByRole('article')).toBeNull()
  expect(article.closest('[hidden]')).not.toBeNull()
  // Closing the results shows it again.
  fireEvent.click(within(results).getByRole('button', { name: 'Close' }))
  expect(screen.getByRole('article', { name: 'ideas.md' })).toBe(article)
})

test('a search made while editing keeps the unsaved text, and Back to the results keeps it too', async () => {
  const calls = kb('write', searchRoutes)
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: /^ideas\.md/ }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  fireEvent.click(await within(article).findByRole('button', { name: 'Edit' }))
  fireEvent.change(within(article).getByLabelText('Text'), { target: { value: 'Unsaved words.' } })

  searchFor('ideas')
  const results = await screen.findByRole('region', { name: 'Search results' })
  await within(results).findByRole('button', { name: 'Ideas' })
  expect(screen.queryByRole('article')).toBeNull()
  // The search's Close brings the edit back as it was.
  fireEvent.click(within(results).getByRole('button', { name: 'Close' }))
  expect((within(article).getByLabelText('Text') as HTMLTextAreaElement).value).toBe('Unsaved words.')

  // Searching again and opening the same document from the results: the edit is there, and Back
  // to the results, then their Close, keep it too.
  searchFor('ideas')
  const again = await screen.findByRole('region', { name: 'Search results' })
  fireEvent.click(await within(again).findByRole('button', { name: 'Ideas' }))
  expect(screen.getByRole('article', { name: 'ideas.md' })).toBe(article)
  expect((within(article).getByLabelText('Text') as HTMLTextAreaElement).value).toBe('Unsaved words.')
  fireEvent.click(within(article).getByRole('button', { name: 'Back to the results' }))
  expect(screen.queryByRole('article')).toBeNull()
  const third = await screen.findByRole('region', { name: 'Search results' })
  fireEvent.click(within(third).getByRole('button', { name: 'Close' }))
  expect((within(article).getByLabelText('Text') as HTMLTextAreaElement).value).toBe('Unsaved words.')
  expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(0)
})

test('a delete from a search result is said beside the results, with the keyboard on its Restore', async () => {
  kb('write', { ...searchRoutes, [`DELETE ${S}/docs/ideas.md`]: { body: { commit: 'c3' } } })
  render(<KnowledgePage workspace={1} />)
  await screen.findByRole('list', { name: 'Documents' })
  searchFor('ideas')
  const results = await screen.findByRole('region', { name: 'Search results' })
  fireEvent.click(await within(results).findByRole('button', { name: 'Ideas' }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  await within(article).findByRole('button', { name: 'Edit' })
  fireEvent.click(within(article).getByRole('button', { name: 'More actions' }))
  fireEvent.click(within(article).getByRole('menuitem', { name: 'Delete' }))
  fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Delete' }))
  const said = await screen.findByText(/Deleted ideas\.md/)
  // In the pane that shows (the results'), not the list's, which a narrow page hides.
  const pane = await screen.findByRole('region', { name: 'Search results' })
  expect(said.closest('.ui-page')).toBe(pane.closest('.ui-page'))
  expect(said.closest('.kb-side')).toBeNull()
  expect(said.closest('[hidden]')).toBeNull()
  expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Restore' }))
})

test('cancelling the delete question deletes nothing', async () => {
  const calls = kb('write', { [`DELETE ${S}/docs/ideas.md`]: { body: { commit: 'c3' } } })
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: /^ideas\.md/ }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  await within(article).findByRole('button', { name: 'Edit' })
  fireEvent.click(within(article).getByRole('button', { name: 'More actions' }))
  fireEvent.click(within(article).getByRole('menuitem', { name: 'Delete' }))
  fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Cancel' }))
  expect(screen.queryByRole('alertdialog')).toBeNull()
  expect(screen.getByRole('article', { name: 'ideas.md' })).toBe(article)
  expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(0)
})
