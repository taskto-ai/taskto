// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { type Place, PlaceContext } from '../router'
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

const folder = (path: string) => {
  const name = [...document.querySelectorAll<HTMLElement>('[data-name-of]')].find(
    (el) => el.dataset.nameOf === `folder:${path}`,
  )
  const button = name?.closest('button')
  if (!button) throw new Error(`no folder ${path} in the tree`)
  return button
}
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
  expect(heading(article).textContent).toBe('Team/notes/plan.md')
  expect(within(article).getByText('7 B')).toBeTruthy()
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
  const status = () => screen.getAllByRole('status').find((el) => el.textContent === 'Loading…')
  await waitFor(() => expect(status()).toBeTruthy())
  expect(screen.getAllByText('Loading…')).toHaveLength(1)
  release()
  await screen.findByRole('list', { name: 'Documents' })
  await screen.findByRole('heading', { name: 'Ideas' })
  await waitFor(() => expect(screen.queryByText('Loading…')).toBeNull())
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
  expect((within(article).getByRole('checkbox', { name: 'Done' }) as HTMLInputElement).disabled).toBe(true)
  expect(within(article).getByText('<b>raw</b>')).toBeTruthy()
  expect(article.querySelector('b')).toBeNull()
})

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
  expect(tree.files.map((d) => d.path)).toContain('a')
  const a = tree.folders.find((f) => f.name === 'a') as Folder
  expect(a.files.map((d) => d.path)).toEqual(['a/b.md'])
  expect(a.folders.map((f) => f.path)).toEqual(['a/b'])
  expect(a.count).toBe(2)
  let at = tree
  for (let i = 0; i < 16; i++) at = at.folders.find((f) => f.name === `d${i}`) as Folder
  expect(at.files.map((d) => d.path)).toEqual([`${deep}/bottom.md`])
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
    else Reflect.deleteProperty(HTMLElement.prototype, 'scrollWidth')
    if (cw) Object.defineProperty(HTMLElement.prototype, 'clientWidth', cw)
    else Reflect.deleteProperty(HTMLElement.prototype, 'clientWidth')
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
  expect(screen.queryByRole('article')).toBeNull()
  expect(article.closest('[hidden]')).not.toBeNull()
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
  fireEvent.click(within(results).getByRole('button', { name: 'Close' }))
  expect((within(article).getByLabelText('Text') as HTMLTextAreaElement).value).toBe('Unsaved words.')

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
  const pane = await screen.findByRole('region', { name: 'Search results' })
  expect(pane.closest('[hidden]')).toBeNull()
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

const asked = () => screen.queryByRole('alertdialog', { name: 'Discard your changes' })

test('a search, its Close and Back to the results ask nothing while editing: the draft waits behind them', async () => {
  kb('write', searchRoutes)
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: /^ideas\.md/ }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  fireEvent.click(await within(article).findByRole('button', { name: 'Edit' }))
  fireEvent.change(within(article).getByLabelText('Text'), { target: { value: 'Unsaved words.' } })
  searchFor('ideas')
  const results = await screen.findByRole('region', { name: 'Search results' })
  expect(asked()).toBeNull()
  fireEvent.click(within(results).getByRole('button', { name: 'Close' }))
  expect(asked()).toBeNull()
  searchFor('ideas')
  fireEvent.click(
    await within(await screen.findByRole('region', { name: 'Search results' })).findByRole('button', { name: 'Ideas' }),
  )
  fireEvent.click(within(article).getByRole('button', { name: 'Back to the results' }))
  expect(asked()).toBeNull()
  expect((within(article).getByLabelText('Text') as HTMLTextAreaElement).value).toBe('Unsaved words.')
})

test('another result, opened while a draft waits behind the results, asks with the draft in front', async () => {
  kb('write', {
    ...searchRoutes,
    [`GET ${S}/search`]: {
      body: {
        results: [ideasResult, { ...ideasResult, path: 'notes/plan.md', title: 'Plan' }],
        mode: 'fulltext',
      },
    },
  })
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: /^ideas\.md/ }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  fireEvent.click(await within(article).findByRole('button', { name: 'Edit' }))
  const text = within(article).getByLabelText('Text') as HTMLTextAreaElement
  fireEvent.change(text, { target: { value: 'Unsaved words.' } })
  text.setSelectionRange(3, 7)
  searchFor('plan')
  const results = await screen.findByRole('region', { name: 'Search results' })
  fireEvent.click(await within(results).findByRole('button', { name: 'Plan' }))

  const question = asked() as HTMLElement
  expect(question).not.toBeNull()
  expect(article.closest('[hidden]')).toBeNull()
  expect(results.closest('[hidden]')).not.toBeNull()
  expect(document.activeElement).toBe(within(question).getByRole('button', { name: 'Keep editing' }))

  fireEvent.click(within(question).getByRole('button', { name: 'Keep editing' }))
  expect(asked()).toBeNull()
  expect(article.closest('[hidden]')).toBeNull()
  expect(document.activeElement).toBe(text)
  expect([text.selectionStart, text.selectionEnd]).toEqual([3, 7])

  searchFor('plan')
  fireEvent.click(
    await within(await screen.findByRole('region', { name: 'Search results' })).findByRole('button', { name: 'Plan' }),
  )
  fireEvent.click(within(asked() as HTMLElement).getByRole('button', { name: 'Discard my changes' }))
  await screen.findByRole('article', { name: 'notes/plan.md' })
})

test('closing the upload dialog leaves the upload going; only its Cancel stops it', async () => {
  let release: (v: unknown) => void = () => {}
  const held = new Promise((resolve) => {
    release = resolve
  })
  const calls = kb('write', {
    [`PUT ${S}/docs/logo.png`]: () =>
      held.then(() => ({ status: 201, body: { commit: 'c1', doc: { path: 'logo.png', version: V2, size: 3 } } })),
  })
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Upload' }))
  const dialog = screen.getByRole('dialog', { name: 'Upload a file' })
  fireEvent.change(within(dialog).getByLabelText('File'), {
    target: { files: [new File([new Uint8Array([1, 2, 3])], 'logo.png')] },
  })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Upload' }))
  await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true))
  fireEvent.click(within(dialog).getByRole('button', { name: 'Close' }))
  expect(screen.queryByRole('dialog')).toBeNull()
  expect(calls.find((c) => c.method === 'PUT')?.signal?.aborted).toBe(false)
  release(undefined)
  await waitFor(() => expect(calls.filter((c) => c.path === `${S}/docs?limit=1000`)).toHaveLength(2))
  expect(screen.queryByRole('article')).toBeNull()
})

test('Back to the results finds every page of them that was loaded, without asking again', async () => {
  const calls = kb('read', {
    ...searchRoutes,
    [`GET ${S}/search?q=ideas`]: { body: { results: [ideasResult], mode: 'fulltext', next_offset: 1 } },
    [`GET ${S}/search?q=ideas&offset=1`]: {
      body: { results: [{ ...ideasResult, path: 'notes/plan.md', title: 'Plan' }], mode: 'fulltext' },
    },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByRole('list', { name: 'Documents' })
  searchFor('ideas')
  const results = await screen.findByRole('region', { name: 'Search results' })
  fireEvent.click(await within(results).findByRole('button', { name: 'More results' }))
  await within(results).findByRole('button', { name: 'Plan' })
  fireEvent.click(within(results).getByRole('button', { name: 'Ideas' }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  fireEvent.click(within(article).getByRole('button', { name: 'Back to the results' }))
  const again = await screen.findByRole('region', { name: 'Search results' })
  expect(within(again).getByRole('button', { name: 'Plan' })).toBeTruthy()
  expect(calls.filter((c) => c.path.startsWith(`${S}/search?`))).toHaveLength(2)
})

test('a restore from the notice is the same one whichever pane comes in front while it runs', async () => {
  let release: (v: unknown) => void = () => {}
  const held = new Promise((resolve) => {
    release = resolve
  })
  kb('write', {
    ...searchRoutes,
    [`DELETE ${S}/docs/ideas.md`]: { body: { commit: 'c3' } },
    [`GET ${S}/history/ideas.md`]: {
      body: {
        commits: [
          { commit: 'c3', change: 'delete' },
          { commit: 'c2', change: 'update', version: V1 },
        ],
      },
    },
    [`POST ${S}/restore/ideas.md`]: () => held.then(() => ({ body: { commit: 'c4', doc: doc('ideas.md') } })),
  })
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: /^ideas\.md/ }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  await within(article).findByRole('button', { name: 'More actions' })
  fireEvent.click(within(article).getByRole('button', { name: 'More actions' }))
  fireEvent.click(within(article).getByRole('menuitem', { name: 'Delete' }))
  fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Delete' }))
  const restore = await screen.findByRole('button', { name: /Restore/ })
  fireEvent.click(restore)
  await waitFor(() => expect(restore.getAttribute('aria-busy')).toBe('true'))
  searchFor('ideas')
  await screen.findByRole('region', { name: 'Search results' })
  expect(screen.getByRole('button', { name: /Restore/ })).toBe(restore)
  expect(restore.getAttribute('aria-busy')).toBe('true')
  release(undefined)
})

test('a document opened from a result is shown in the tree, even in a folder that was closed', async () => {
  kb('read', {
    ...searchRoutes,
    [`GET ${S}/search`]: {
      body: { results: [{ ...ideasResult, path: 'notes/plan.md', title: 'Plan' }], mode: 'fulltext' },
    },
  })
  render(<KnowledgePage workspace={1} open="3:notes/plan.md" />)
  await screen.findByRole('article', { name: 'notes/plan.md' })
  await screen.findByRole('list', { name: 'Documents' })
  fireEvent.click(folder('notes'))
  expect(folder('notes').getAttribute('aria-expanded')).toBe('false')
  searchFor('plan')
  const results = await screen.findByRole('region', { name: 'Search results' })
  fireEvent.click(await within(results).findByRole('button', { name: 'Plan' }))
  await waitFor(() => expect(folder('notes').getAttribute('aria-expanded')).toBe('true'))
})

test('Back to the results gives the keyboard to the result that opened the document', async () => {
  kb('read', {
    ...searchRoutes,
    [`GET ${S}/search`]: {
      body: { results: [{ ...ideasResult, path: 'notes/plan.md', title: 'Plan' }, ideasResult], mode: 'fulltext' },
    },
  })
  render(<KnowledgePage workspace={1} />)
  await screen.findByRole('list', { name: 'Documents' })
  searchFor('ideas')
  const results = await screen.findByRole('region', { name: 'Search results' })
  const second = await within(results).findByRole('button', { name: 'Ideas' })
  second.focus()
  fireEvent.click(second)
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  fireEvent.click(within(article).getByRole('button', { name: 'Back to the results' }))
  await screen.findByRole('region', { name: 'Search results' })
  expect(document.activeElement).toBe(second)
})

test('Try again on a document that failed keeps the keyboard in the document', async () => {
  let fail = true
  kb('read', {
    [`GET ${S}/docs/ideas.md`]: () =>
      fail ? { status: 500, body: { error: 'disk on fire' } } : { body: { ...doc('ideas.md'), content: '# Ideas\n' } },
  })
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: /^ideas\.md/ }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  const again = await within(article).findByRole('button', { name: 'Try again' })
  again.focus()
  fail = false
  fireEvent.click(again)
  expect(document.activeElement).toBe(heading(article))
  await within(article).findByRole('heading', { name: 'Ideas' })
  expect(document.activeElement).toBe(heading(article))
})

test('Try again on a history that failed keeps the keyboard in the history', async () => {
  let fail = true
  kb('read', {
    [`GET ${S}/history/ideas.md`]: () =>
      fail ? { status: 500, body: { error: 'disk on fire' } } : { body: { commits: [] } },
  })
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: /^ideas\.md/ }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  fireEvent.click(await within(article).findByRole('button', { name: 'History' }))
  const history = within(article).getByRole('region', { name: 'History' })
  const again = await within(history).findByRole('button', { name: 'Try again' })
  again.focus()
  fail = false
  fireEvent.click(again)
  expect(document.activeElement).toBe(history)
  await waitFor(() => expect(within(history).queryByRole('button', { name: 'Try again' })).toBeNull())
  expect(document.activeElement).toBe(history)
})

test('opening the open document while a save is in flight drops another opening waiting for the save', async () => {
  let release: (v: unknown) => void = () => {}
  const held = new Promise((resolve) => {
    release = resolve
  })
  kb('write', {
    ...searchRoutes,
    [`GET ${S}/search`]: {
      body: { results: [{ ...ideasResult, path: 'notes/plan.md', title: 'Plan' }], mode: 'fulltext' },
    },
    [`PUT ${S}/docs/ideas.md`]: () =>
      held.then(() => ({ body: { commit: 'c2', doc: { ...doc('ideas.md'), version: V2 } } })),
  })
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: /^ideas\.md/ }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  fireEvent.click(await within(article).findByRole('button', { name: 'Edit' }))
  fireEvent.change(within(article).getByLabelText('Text'), { target: { value: 'New words.' } })
  fireEvent.click(within(article).getByRole('button', { name: 'Save' }))
  searchFor('plan')
  const results = await screen.findByRole('region', { name: 'Search results' })
  fireEvent.click(await within(results).findByRole('button', { name: 'Plan' }))
  fireEvent.click(screen.getByRole('button', { name: /^ideas\.md/ }))
  release(undefined)
  await waitFor(() => expect(within(article).queryByLabelText('Text')).toBeNull())
  await new Promise((resolve) => setTimeout(resolve, 20))
  expect(screen.getByRole('article', { name: 'ideas.md' })).toBe(article)
  expect(screen.queryByRole('article', { name: 'notes/plan.md' })).toBeNull()
})

test('the knowledge base reports its title to its place, and withdraws it with the page', async () => {
  kb('write')
  const titles: (string | null)[] = []
  const at: Place = { id: 'a', path: '/w/1/knowledge', navigate: vi.fn(), setTitle: (title) => titles.push(title) }
  const view = render(
    <PlaceContext.Provider value={at}>
      <KnowledgePage workspace={1} />
    </PlaceContext.Provider>,
  )
  await screen.findByRole('button', { name: /^ideas\.md/ })
  expect(titles.at(-1)).toBe('Knowledge base')
  view.unmount()
  expect(titles.at(-1)).toBeNull()
})

test('a dirty editor in one place intercepts its own links, and leaves another place’s clicks alone', async () => {
  kb('write')
  const place = (id: string): Place => ({ id, path: '/w/1/knowledge', navigate: vi.fn(), setTitle: vi.fn() })
  const a = place('a')
  const b = place('b')
  const seen: boolean[] = []
  render(
    <>
      <PlaceContext.Provider value={a}>
        <div data-place="a">
          <a href="/w/1/apps" onClick={(e) => e.preventDefault()}>
            Apps
          </a>
          <KnowledgePage workspace={1} />
        </div>
      </PlaceContext.Provider>
      <PlaceContext.Provider value={b}>
        <div data-place="b">
          <a
            href="/w/1/boards"
            onClick={(e) => {
              seen.push(e.defaultPrevented)
              e.preventDefault()
            }}
          >
            Boards
          </a>
        </div>
      </PlaceContext.Provider>
    </>,
  )
  fireEvent.click(await screen.findByRole('button', { name: /^ideas\.md/ }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  fireEvent.click(await within(article).findByRole('button', { name: 'Edit' }))
  fireEvent.change(within(article).getByLabelText('Text'), { target: { value: 'Unsaved words.' } })

  fireEvent.click(screen.getByRole('link', { name: 'Boards' }))
  expect(seen).toEqual([false])
  expect(asked()).toBeNull()
  expect(a.navigate).not.toHaveBeenCalled()

  fireEvent.click(screen.getByRole('link', { name: 'Apps' }))
  expect(asked()).toBeTruthy()
  fireEvent.click(within(article).getByRole('button', { name: 'Discard my changes' }))
  expect(a.navigate).toHaveBeenCalledWith('/w/1/apps')
  expect(b.navigate).not.toHaveBeenCalled()
  expect(window.location.pathname).toBe('/')
})

test('a link to the page of its own place is not intercepted, whatever the browser’s address is', async () => {
  kb('write')
  const at: Place = { id: 'a', path: '/w/1/knowledge', navigate: vi.fn(), setTitle: vi.fn() }
  const seen: boolean[] = []
  render(
    <PlaceContext.Provider value={at}>
      <div data-place="a">
        <a
          href="/w/1/knowledge"
          onClick={(e) => {
            seen.push(e.defaultPrevented)
            e.preventDefault()
          }}
        >
          Knowledge page
        </a>
        <KnowledgePage workspace={1} />
      </div>
    </PlaceContext.Provider>,
  )
  fireEvent.click(await screen.findByRole('button', { name: /^ideas\.md/ }))
  const article = await screen.findByRole('article', { name: 'ideas.md' })
  fireEvent.click(await within(article).findByRole('button', { name: 'Edit' }))
  fireEvent.change(within(article).getByLabelText('Text'), { target: { value: 'Unsaved words.' } })

  fireEvent.click(screen.getByRole('link', { name: 'Knowledge page' }))
  expect(seen).toEqual([false])
  expect(asked()).toBeNull()
  expect(at.navigate).not.toHaveBeenCalled()
})
