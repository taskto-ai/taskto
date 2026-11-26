// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { BoardsPage } from '../boards/BoardsPage'
import { beginSession, endSession } from '../session'
import { fakeFetch } from '../test/fakes'
import { resolveDocLink } from './docs'
import { KnowledgePage } from './KnowledgePage'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

test('a workspace without spaces says so, rather than loading forever', async () => {
  fakeFetch({ 'GET /api/workspaces/1/spaces': { body: { spaces: [] } } })
  render(<KnowledgePage workspace={1} />)
  expect(await screen.findByText('You have no spaces in this workspace.')).toBeTruthy()
  expect(screen.queryByText('Loading…')).toBeNull()
  cleanup()
  render(<BoardsPage workspace={1} />)
  expect(await screen.findByText('You have no spaces in this workspace.')).toBeTruthy()
  expect(screen.queryByText('Loading…')).toBeNull()
})

const S = '/api/workspaces/1/spaces/3'
const V1 = 'a'.repeat(40)
const V2 = 'b'.repeat(40)
const space = (access: 'write' | 'read', id = 3, name = 'Team') => ({
  id,
  workspace_id: 1,
  kind: 'shared',
  name,
  archived_at: null,
  role: access === 'write' ? 'editor' : 'viewer',
  access,
})
const plan = { path: 'notes/plan.md', version: V1, size: 7, versioned: true }

// The routes a space with one document needs; `extra` adds or overrides.
function kb(access: 'write' | 'read', extra: Parameters<typeof fakeFetch>[0] = {}) {
  return fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces: [space(access)] } },
    [`GET ${S}/docs`]: { body: { docs: [plan] } },
    [`GET ${S}/docs/notes/plan.md`]: { body: { ...plan, content: '# Plan\n\nThe **first** draft.\n' } },
    ...extra,
  })
}

// The tree's folder "notes" is closed until it is opened, then its document.
async function openPlan() {
  fireEvent.click(await screen.findByRole('button', { name: /^notes/ }))
  fireEvent.click(screen.getByRole('button', { name: /^plan\.md/ }))
  return screen.findByRole('article', { name: 'notes/plan.md' })
}

// One of the document's actions in its "More actions" menu.
function fromMenu(doc: HTMLElement, name: string) {
  fireEvent.click(within(doc).getByRole('button', { name: 'More actions' }))
  fireEvent.click(within(doc).getByRole('menuitem', { name }))
}

test('a viewer reads documents and their history, but gets no way to change them', async () => {
  kb('read', {
    [`GET ${S}/history/notes/plan.md`]: {
      body: {
        commits: [
          {
            commit: 'c2',
            author: { name: 'Ann', email: 'a@x' },
            time: '2026-01-02T00:00:00Z',
            message: 'Update',
            change: 'update',
            version: V1,
          },
          {
            commit: 'c1',
            author: { name: 'Ann', email: 'a@x' },
            time: '2026-01-01T00:00:00Z',
            message: 'Create',
            change: 'create',
            version: V2,
          },
        ],
      },
    },
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  // The preview is Markdown, made into elements.
  expect((await within(doc).findByRole('heading', { name: 'Plan' })).tagName).toBe('H1')
  expect(within(doc).getByText('first').tagName).toBe('STRONG')
  for (const name of ['New', 'Upload', 'Edit', 'Move', 'Delete']) {
    expect(screen.queryByRole('button', { name })).toBeNull()
  }
  fireEvent.click(within(doc).getByRole('button', { name: 'History' }))
  await within(doc).findByText('Create')
  expect(within(doc).getAllByRole('button', { name: 'View' })).toHaveLength(2)
  expect(within(doc).queryByRole('button', { name: 'Restore this version' })).toBeNull()
})

test('older history loads one page at a time, however often it is clicked', async () => {
  const commit = (id: string, message: string) => ({
    commit: id,
    author: { name: 'Ann', email: 'a@x' },
    time: '2026-01-01T00:00:00Z',
    message,
    change: 'update',
    version: V1,
  })
  const calls = kb('read', {
    [`GET ${S}/history/notes/plan.md`]: { body: { commits: [commit('c3', 'Third')], next: 'c3' } },
    [`GET ${S}/history/notes/plan.md?before=c3`]: { body: { commits: [commit('c2', 'Second')] } },
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  fireEvent.click(within(doc).getByRole('button', { name: 'History' }))
  const older = await within(doc).findByRole('button', { name: 'Older' })
  fireEvent.click(older)
  fireEvent.click(older)
  await within(doc).findByText('Second')
  expect(within(doc).getAllByText('Second')).toHaveLength(1)
  expect(calls.filter((c) => c.path.endsWith('?before=c3'))).toHaveLength(1)
})

test('saving an edit sends the version it started from', async () => {
  const calls = kb('write', {
    [`PUT ${S}/docs/notes/plan.md`]: { body: { commit: 'c3', doc: { ...plan, version: V2 } } },
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  fireEvent.click(await within(doc).findByRole('button', { name: 'Edit' }))
  fireEvent.change(within(doc).getByLabelText('Text'), { target: { value: '# Plan\n\nBetter.\n' } })
  fireEvent.click(within(doc).getByRole('radio', { name: 'Preview' }))
  expect(within(doc).getByText('Better.').tagName).toBe('P')
  fireEvent.change(within(doc).getByLabelText('What changed (optional)'), { target: { value: 'Tighten' } })
  fireEvent.click(within(doc).getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(within(doc).queryByRole('button', { name: 'Save' })).toBeNull())
  expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({
    content: '# Plan\n\nBetter.\n',
    base_version: V1,
    message: 'Tighten',
  })
})

test('a stale save is a conflict to settle: the edit is kept, and overwriting is a choice', async () => {
  let current = { ...plan, version: V2, content: '# Plan\n\nTheirs.\n' }
  const calls = kb('write', {
    [`GET ${S}/docs/notes/plan.md`]: () => ({ body: current }),
    [`PUT ${S}/docs/notes/plan.md`]: (call) => {
      if ((call.body as { base_version: string }).base_version !== current.version) {
        return { status: 409, body: { error: 'conflict' } }
      }
      current = { ...current, version: 'c'.repeat(40), content: (call.body as { content: string }).content }
      return { body: { commit: 'c9', doc: current } }
    },
  })
  // The page opened the document before someone else saved it.
  current = { ...plan, version: V1, content: '# Plan\n' }
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  fireEvent.click(await within(doc).findByRole('button', { name: 'Edit' }))
  current = { ...plan, version: V2, content: '# Plan\n\nTheirs.\n' }

  const text = within(doc).getByLabelText('Text') as HTMLTextAreaElement
  fireEvent.change(text, { target: { value: 'Mine.' } })
  fireEvent.click(within(doc).getByRole('button', { name: 'Save' }))
  expect((await within(doc).findByRole('alert')).textContent).toContain('Someone changed this document')
  expect(text.value).toBe('Mine.')
  expect((within(doc).getByRole('button', { name: 'Save' }) as HTMLButtonElement).disabled).toBe(true)

  // Loading theirs replaces the text, and saving then is based on their version.
  fireEvent.click(within(doc).getByRole('button', { name: 'Load their version' }))
  expect(text.value).toBe('# Plan\n\nTheirs.\n')
  fireEvent.change(text, { target: { value: 'Mine again.' } })
  current = { ...plan, version: 'd'.repeat(40), content: 'Third.' }
  fireEvent.click(within(doc).getByRole('button', { name: 'Save' }))
  await within(doc).findByRole('button', { name: 'Overwrite with mine' })

  // Overwriting is deliberate: it names the version it replaces.
  fireEvent.click(within(doc).getByRole('button', { name: 'Overwrite with mine' }))
  await waitFor(() => expect(current.content).toBe('Mine again.'))
  const puts = calls.filter((c) => c.method === 'PUT').map((c) => (c.body as { base_version: string }).base_version)
  expect(puts).toEqual([V1, V2, 'd'.repeat(40)])
})

test("a stale save whose document can't be reloaded offers no overwrite of an unseen version", async () => {
  let reads = 0
  kb('write', {
    [`GET ${S}/docs/notes/plan.md`]: () =>
      reads++ === 0 ? { body: { ...plan, content: '# Plan\n' } } : { status: 500, body: { error: 'disk on fire' } },
    [`PUT ${S}/docs/notes/plan.md`]: { status: 409, body: { error: 'conflict' } },
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  fireEvent.click(await within(doc).findByRole('button', { name: 'Edit' }))
  fireEvent.change(within(doc).getByLabelText('Text'), { target: { value: 'Mine.' } })
  fireEvent.click(within(doc).getByRole('button', { name: 'Save' }))
  expect((await within(doc).findByRole('alert')).textContent).toContain("couldn't be reloaded: disk on fire")
  expect(within(doc).queryByRole('button', { name: /Overwrite|Save mine again/ })).toBeNull()
})

test('creates a Markdown document and opens it to edit', async () => {
  const calls = kb('write', {
    [`PUT ${S}/docs/ideas/launch.md`]: {
      status: 201,
      body: { commit: 'c1', doc: { path: 'ideas/launch.md', version: V2, size: 9 } },
    },
    [`GET ${S}/docs/ideas/launch.md`]: {
      body: { path: 'ideas/launch.md', version: V2, size: 9, content: '# launch\n' },
    },
  })
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: 'New' }))
  const dialog = screen.getByRole('dialog', { name: 'New document' })
  expect(document.activeElement).toBe(within(dialog).getByLabelText('Name'))
  fireEvent.change(within(dialog).getByLabelText('Name'), { target: { value: 'ideas/launch' } })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Create' }))
  const doc = await screen.findByRole('article', { name: 'ideas/launch.md' })
  const text = (await within(doc).findByLabelText('Text')) as HTMLTextAreaElement
  expect(text.value).toBe('# launch\n')
  // Opened to write: the keyboard is in the text, and Save is the page's one primary button.
  expect(document.activeElement).toBe(text)
  expect(screen.queryByRole('dialog')).toBeNull()
  await waitFor(() => expect(document.querySelectorAll('.ui-button-primary')).toHaveLength(1))
  expect(within(doc).getByRole('button', { name: 'Save' }).classList).toContain('ui-button-primary')
  // Creating never names a version: an existing document is a 409, not replaced.
  expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ content: '# launch\n' })
})

test('moves a document, naming its version', async () => {
  const calls = kb('write', {
    [`PATCH ${S}/docs/notes/plan.md`]: { body: { commit: 'c2', doc: { ...plan, path: 'archive/plan.md' } } },
    [`GET ${S}/docs/archive/plan.md`]: { body: { ...plan, path: 'archive/plan.md', content: 'moved' } },
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  await within(doc).findByRole('button', { name: 'Edit' })
  fromMenu(doc, 'Move')
  const dialog = within(doc).getByRole('dialog', { name: 'Move the document' })
  fireEvent.change(within(dialog).getByLabelText('New name'), { target: { value: 'archive/plan.md' } })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Move' }))
  const moved = await screen.findByRole('article', { name: 'archive/plan.md' })
  expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ to: 'archive/plan.md', base_version: V1 })
  // The keyboard goes to the moved document, not to the top of the page.
  await waitFor(() => expect(document.activeElement).toBe(within(moved).getByRole('heading', { level: 2 })))
})

test('deletes a document, then restores it from its history', async () => {
  const calls = kb('write', {
    [`DELETE ${S}/docs/notes/plan.md`]: { body: { commit: 'c3' } },
    [`GET ${S}/history/notes/plan.md`]: {
      body: {
        commits: [
          {
            commit: 'c3',
            author: { name: 'A', email: 'a@x' },
            time: '2026-01-03T00:00:00Z',
            message: 'Delete',
            change: 'delete',
          },
          {
            commit: 'c2',
            author: { name: 'A', email: 'a@x' },
            time: '2026-01-02T00:00:00Z',
            message: 'Update',
            change: 'update',
            version: V1,
          },
        ],
      },
    },
    [`POST ${S}/restore/notes/plan.md`]: { body: { commit: 'c4', doc: plan } },
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  await within(doc).findByRole('button', { name: 'Edit' })
  fromMenu(doc, 'Delete')
  fireEvent.click(
    within(screen.getByRole('alertdialog', { name: 'Delete the document' })).getByRole('button', { name: 'Delete' }),
  )
  await screen.findByText(/Deleted notes\/plan\.md/)
  expect(screen.queryByRole('article')).toBeNull()
  expect(calls.find((c) => c.method === 'DELETE')?.body).toEqual({ base_version: V1 })
  // The keyboard is on the way back.
  const restore = screen.getByRole('button', { name: 'Restore' })
  expect(document.activeElement).toBe(restore)

  fireEvent.click(restore)
  const back = await screen.findByRole('article', { name: 'notes/plan.md' })
  await waitFor(() => expect(document.activeElement).toBe(within(back).getByRole('heading', { level: 2 })))
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ commit: 'c2' })
  // It's back: the notice, and its Restore button, are gone.
  expect(screen.queryByText(/Deleted notes\/plan\.md/)).toBeNull()
})

test('restores an earlier version, and a stale restore says so with a reload', async () => {
  let stale = true
  const calls = kb('write', {
    [`GET ${S}/history/notes/plan.md`]: {
      body: {
        commits: [
          {
            commit: 'c2',
            author: { name: 'A', email: 'a@x' },
            time: '2026-01-02T00:00:00Z',
            message: 'Update',
            change: 'update',
            version: V1,
          },
          {
            commit: 'c1',
            author: { name: 'A', email: 'a@x' },
            agent: 'taskto.ai agent',
            time: '2026-01-01T00:00:00Z',
            message: 'Create',
            change: 'create',
            version: V2,
          },
        ],
      },
    },
    [`GET ${S}/docs/notes/plan.md?at=c1`]: { body: { ...plan, version: V2, content: 'The *old* text' } },
    [`POST ${S}/restore/notes/plan.md`]: () =>
      stale ? { status: 409, body: { error: 'conflict' } } : { body: { commit: 'c3', doc: plan } },
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  expect(await within(doc).findByText(/with taskto\.ai agent/)).toBeTruthy()
  expect(within(doc).getByText('Current')).toBeTruthy()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'View' })[1])
  expect((await within(doc).findByText('old')).tagName).toBe('EM')

  // Restoring asks first; cancelling sends nothing.
  fireEvent.click(within(doc).getByRole('button', { name: 'Restore this version' }))
  fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Cancel' }))
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)
  const confirmRestore = async () => {
    fireEvent.click(await within(doc).findByRole('button', { name: 'Restore this version' }))
    const ask = screen.getByRole('alertdialog', { name: 'Restore this version?' })
    expect(ask.textContent).toContain('notes/plan.md becomes again what it was on')
    fireEvent.click(within(ask).getByRole('button', { name: 'Restore this version' }))
  }
  await confirmRestore()
  const alert = await within(doc).findByRole('alert')
  expect(alert.textContent).toContain('changed since you opened it')
  stale = false
  fireEvent.click(within(alert).getByRole('button', { name: 'Reload' }))
  await confirmRestore()
  await waitFor(() => expect(within(doc).queryByRole('region', { name: 'History' })).toBeNull())
  // Restored: the keyboard goes to the document.
  expect(document.activeElement).toBe(within(doc).getByRole('heading', { level: 2 }))
  expect(calls.filter((c) => c.method === 'POST').map((c) => c.body)).toEqual([
    { commit: 'c1', base_version: V1 },
    { commit: 'c1', base_version: V1 },
  ])
})

test('uploads a file as bytes, and replaces one only when asked', async () => {
  let exists = false
  let gone = false
  const calls = kb('write', {
    [`PUT ${S}/docs/logo.png`]: (call) =>
      exists && !call.path.includes('base_version=')
        ? { status: 409, body: { error: 'conflict' } }
        : { status: 201, body: { commit: 'c1', doc: { path: 'logo.png', version: V2, size: 3 } } },
    [`GET ${S}/docs/logo.png`]: () =>
      gone
        ? { status: 404, body: { error: 'not found' } }
        : { body: { path: 'logo.png', version: V2, size: 3, content_base64: 'AQID' } },
    [`PUT ${S}/docs/readme.md`]: {
      status: 201,
      body: { commit: 'c2', doc: { path: 'readme.md', version: V1, size: 5 } },
    },
    [`GET ${S}/docs/readme.md`]: { body: { path: 'readme.md', version: V1, size: 5, content: 'héllo' } },
  })
  render(<KnowledgePage workspace={1} />)
  const upload = async (file: File) => {
    fireEvent.click(await screen.findByRole('button', { name: 'Upload' }))
    const dialog = screen.getByRole('dialog', { name: 'Upload a file' })
    fireEvent.change(within(dialog).getByLabelText('File'), { target: { files: [file] } })
    expect((within(dialog).getByLabelText('Save as') as HTMLInputElement).value).toBe(file.name)
    fireEvent.click(within(dialog).getByRole('button', { name: 'Upload' }))
    return dialog
  }

  await upload(new File([new Uint8Array([1, 2, 3])], 'logo.png'))
  const file = await screen.findByRole('article', { name: 'logo.png' })
  expect((await within(file).findByRole('link', { name: 'Download' })).getAttribute('href')).toBe(
    `${S}/docs/logo.png?raw=true`,
  )
  const lastPut = () => calls.filter((c) => c.method === 'PUT').at(-1)
  expect(lastPut()?.path).toBe(`${S}/docs/logo.png`)
  expect(lastPut()?.body).toBeInstanceOf(Blob)

  // A Markdown file goes as its text.
  await upload(new File([new TextEncoder().encode('héllo')], 'readme.md'))
  await screen.findByRole('article', { name: 'readme.md' })
  expect(calls.filter((c) => c.method === 'PUT').at(-1)?.body).toEqual({ content: 'héllo' })

  // A name that is taken is refused, until the user chooses to replace it.
  exists = true
  const form = await upload(new File([new Uint8Array([1, 2, 3])], 'logo.png'))
  expect(await within(form).findByText('logo.png already exists.')).toBeTruthy()
  fireEvent.click(within(form).getByRole('button', { name: 'Replace it' }))
  await screen.findByRole('article', { name: 'logo.png' })
  expect(lastPut()?.path).toBe(`${S}/docs/logo.png?base_version=${V2}`)
  expect(lastPut()?.body).toBeInstanceOf(Blob)

  // Deleted between "already exists" and "Replace it": replacing it is creating it.
  const again = await upload(new File([new Uint8Array([1, 2, 3])], 'logo.png'))
  expect(await within(again).findByText('logo.png already exists.')).toBeTruthy()
  exists = false
  gone = true
  fireEvent.click(within(again).getByRole('button', { name: 'Replace it' }))
  await waitFor(() => expect(lastPut()?.path).toBe(`${S}/docs/logo.png`))
  expect(screen.queryByRole('dialog', { name: 'Upload a file' })).toBeNull()
})

test('a link in a document resolves against its folder, and never leaves the space', () => {
  expect(resolveDocLink('notes/2026/a.md', '../plan.md')).toBe('notes/plan.md')
  expect(resolveDocLink('notes/a.md', './b%20c.md#part')).toBe('notes/b c.md')
  expect(resolveDocLink('a.md', '../outside.md')).toBeNull()
  expect(resolveDocLink('a.md', '/w/1/boards')).toBeNull()
  expect(resolveDocLink('a.md', '#top')).toBeNull()
})

test('a conflict the server names says what happened; another says what may have', async () => {
  let code: string | undefined = 'doc_exists'
  kb('write', {
    [`PATCH ${S}/docs/notes/plan.md`]: () => ({
      status: 409,
      body: { error: 'conflict: archive/plan.md already exists', code, params: { path: 'archive/plan.md' } },
    }),
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  await within(doc).findByRole('button', { name: 'Edit' })
  fromMenu(doc, 'Move')
  const dialog = within(doc).getByRole('dialog', { name: 'Move the document' })
  fireEvent.change(within(dialog).getByLabelText('New name'), { target: { value: 'archive/plan.md' } })
  const move = within(dialog).getByRole('button', { name: 'Move' })
  fireEvent.click(move)
  const said = (await within(dialog).findByRole('alert')).textContent
  expect(said).toContain('archive/plan.md already exists.')
  expect(said).not.toContain('may have changed')
  code = undefined
  fireEvent.click(move)
  await waitFor(() =>
    expect(within(dialog).getByRole('alert').textContent).toContain('It may have changed since you opened it'),
  )
})

test('an unversioned file plays, says it is not versioned, and its history and deletion say so too', async () => {
  const talk = { path: 'talks/intro.mp4', version: V2, size: 80_000_000, versioned: false }
  const calls = kb('write', {
    [`GET ${S}/docs`]: { body: { docs: [plan, talk] } },
    [`GET ${S}/docs/talks/intro.mp4`]: { body: talk },
    [`GET ${S}/history/talks/intro.mp4`]: {
      body: {
        commits: [
          {
            commit: 'c2',
            author: { name: 'Ann', email: 'a@x' },
            time: '2026-01-02T00:00:00Z',
            message: 'Update',
            change: 'update',
            version: V2,
          },
          {
            commit: 'c1',
            author: { name: 'Ann', email: 'a@x' },
            time: '2026-01-01T00:00:00Z',
            message: 'Create',
            change: 'create',
            version: V1,
          },
        ],
      },
    },
    [`DELETE ${S}/docs/talks/intro.mp4`]: { body: { commit: 'c3' } },
  })
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: /^talks/ }))
  const item = screen.getByRole('button', { name: /^intro\.mp4/ })
  expect(item.textContent).toContain('80 MB · not versioned')
  fireEvent.click(item)
  const doc = await screen.findByRole('article', { name: 'talks/intro.mp4' })
  const video = await within(doc).findByLabelText('talks/intro.mp4', { selector: 'video' })
  expect(video.getAttribute('src')).toBe(`${S}/docs/talks/intro.mp4?raw=true`)
  expect(within(doc).getByText('Not versioned: only its current version is kept.')).toBeTruthy()

  // History lists who changed it, without viewing or restoring older versions.
  fireEvent.click(within(doc).getByRole('button', { name: 'History' }))
  const history = await within(doc).findByRole('region', { name: 'History' })
  await within(history).findByText(/Older versions can't be viewed or restored/)
  await within(history).findAllByText('Ann', { exact: false })
  expect(within(history).queryByRole('button', { name: 'View' })).toBeNull()
  expect(within(history).queryByRole('button', { name: 'Restore this version' })).toBeNull()

  // Deleting it says it can't come back, and offers no restore.
  fromMenu(doc, 'Delete')
  const ask = within(doc).getByRole('alertdialog', { name: 'Delete the document' })
  expect(within(ask).getByText("Delete talks/intro.mp4? It isn't versioned: it can't be restored.")).toBeTruthy()
  fireEvent.click(within(ask).getByRole('button', { name: 'Delete' }))
  expect(await screen.findByText("Deleted talks/intro.mp4. It wasn't versioned, so it can't come back.")).toBeTruthy()
  expect(screen.queryByRole('button', { name: 'Restore' })).toBeNull()
  expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(1)
})

test("an admin removes a document's old versions from history, after a warning about backups", async () => {
  const entry = (commit: string, version: string, versioned = true) => ({
    commit,
    author: { name: 'A', email: 'a@x' },
    time: '2026-01-02T00:00:00Z',
    message: commit,
    change: 'update',
    version,
    versioned,
  })
  let removed = false
  const history = () => ({
    body: {
      commits: removed
        ? [entry('n3', V1), entry('n2', V2, false), entry('n1', 'c'.repeat(40), false)]
        : [entry('c3', V1), entry('c2', V2, false), entry('c1', 'c'.repeat(40))],
    },
  })
  const calls = kb('write', {
    [`GET ${S}/history/notes/plan.md`]: history,
    [`DELETE ${S}/history/notes/plan.md`]: () => {
      removed = true
      return { body: { commit: 'n4', doc: plan, removed: 1 } }
    },
  })
  render(<KnowledgePage workspace={1} admin />)
  const doc = await openPlan()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  // A version already removed says so, and can't be viewed.
  expect(await within(doc).findByText(/Removed from history/)).toBeTruthy()
  expect(within(doc).getAllByRole('button', { name: 'View' })).toHaveLength(2)

  fireEvent.click(within(doc).getByRole('button', { name: 'Remove old versions from history…' }))
  const panel = within(doc).getByRole('alertdialog', { name: 'Remove the old versions from history' })
  expect(within(panel).getByText(/deleted for good/)).toBeTruthy()
  expect(within(panel).getByText(/Backups made before today still hold the older versions/)).toBeTruthy()
  fireEvent.click(within(panel).getByRole('button', { name: 'Remove old versions' }))
  expect(await within(doc).findByText('Removed 1 old version from the history.')).toBeTruthy()
  expect(calls.filter((c) => c.method === 'DELETE')).toEqual([
    { method: 'DELETE', path: `${S}/history/notes/plan.md`, body: { base_version: V1 } },
  ])
  // The history is loaded again: its commits have new IDs, and nothing is left to remove.
  await waitFor(() => expect(within(doc).getAllByText(/Removed from history/)).toHaveLength(2))
  expect(within(doc).queryByRole('button', { name: 'Remove old versions from history…' })).toBeNull()
})

test('only an admin who can write the space is offered to remove old versions', async () => {
  const commits = [
    {
      commit: 'c2',
      author: { name: 'A', email: 'a@x' },
      time: '2026-01-02T00:00:00Z',
      message: 'Update',
      version: V1,
    },
    { commit: 'c1', author: { name: 'A', email: 'a@x' }, time: '2026-01-01T00:00:00Z', message: 'Create', version: V2 },
  ]
  for (const [access, admin] of [
    ['write', false],
    ['read', true],
  ] as const) {
    kb(access, { [`GET ${S}/history/notes/plan.md`]: { body: { commits } } })
    render(<KnowledgePage workspace={1} admin={admin} />)
    const doc = await openPlan()
    fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
    await within(doc).findAllByRole('button', { name: 'View' })
    expect(within(doc).queryByRole('button', { name: 'Remove old versions from history…' })).toBeNull()
    cleanup()
  }
})

// --- A restore or an upload whose session ends in the middle -------------------------------

test('a restore whose session ends while it reads the history restores nothing', async () => {
  let history: () => void = () => {}
  const deleted = {
    commits: [
      {
        commit: 'c3',
        author: { name: 'A', email: 'a@x' },
        time: '2026-01-03T00:00:00Z',
        message: 'Delete',
        change: 'delete',
      },
      {
        commit: 'c2',
        author: { name: 'A', email: 'a@x' },
        time: '2026-01-02T00:00:00Z',
        message: 'Update',
        change: 'update',
        version: V1,
      },
    ],
  }
  const calls = kb('write', {
    [`DELETE ${S}/docs/notes/plan.md`]: { body: { commit: 'c3' } },
    [`GET ${S}/history/notes/plan.md`]: () =>
      new Promise((done) => {
        history = () => done({ body: deleted })
      }),
    [`POST ${S}/restore/notes/plan.md`]: { body: { commit: 'c4', doc: plan } },
  })
  beginSession(1)
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  await within(doc).findByRole('button', { name: 'Edit' })
  fromMenu(doc, 'Delete')
  fireEvent.click(
    within(screen.getByRole('alertdialog', { name: 'Delete the document' })).getByRole('button', { name: 'Delete' }),
  )
  fireEvent.click(await screen.findByRole('button', { name: 'Restore' }))
  await waitFor(() => expect(calls.some((c) => c.path === `${S}/history/notes/plan.md`)).toBe(true))
  endSession()
  await act(async () => history())
  await new Promise((r) => setTimeout(r, 20))
  expect(calls.some((c) => c.path.startsWith(`${S}/restore/`))).toBe(false)
})

test('an upload that replaces a file, whose session ends while it reads the current one, sends nothing', async () => {
  let current: () => void = () => {}
  let first = true
  const calls = kb('write', {
    [`PUT ${S}/docs/logo.png`]: (call) =>
      !call.path.includes('base_version=')
        ? { status: 409, body: { error: 'conflict' } }
        : { status: 201, body: { commit: 'c1', doc: { path: 'logo.png', version: V2, size: 3 } } },
    [`GET ${S}/docs/logo.png`]: () => {
      if (first) {
        first = false
        return new Promise((done) => {
          current = () => done({ body: { path: 'logo.png', version: V2, size: 3, content_base64: 'AQID' } })
        })
      }
      return { body: { path: 'logo.png', version: V2, size: 3, content_base64: 'AQID' } }
    },
  })
  beginSession(1)
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Upload' }))
  const dialog = screen.getByRole('dialog', { name: 'Upload a file' })
  fireEvent.change(within(dialog).getByLabelText('File'), {
    target: { files: [new File([new Uint8Array([1, 2, 3])], 'logo.png')] },
  })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Upload' }))
  fireEvent.click(await within(dialog).findByRole('button', { name: 'Replace it' }))
  await waitFor(() =>
    expect(calls.filter((c) => c.method === 'GET' && c.path === `${S}/docs/logo.png`)).toHaveLength(1),
  )
  const puts = () => calls.filter((c) => c.method === 'PUT').length
  const before = puts()
  endSession()
  await act(async () => current())
  await new Promise((r) => setTimeout(r, 20))
  expect(puts()).toBe(before)
})

test('a restore answered after its session ended opens nothing, though the page is still here', async () => {
  let restored: () => void = () => {}
  const calls = kb('write', {
    [`DELETE ${S}/docs/notes/plan.md`]: { body: { commit: 'c3' } },
    [`GET ${S}/history/notes/plan.md`]: {
      body: {
        commits: [
          {
            commit: 'c3',
            author: { name: 'A', email: 'a@x' },
            time: '2026-01-03T00:00:00Z',
            message: 'Delete',
            change: 'delete',
          },
          {
            commit: 'c2',
            author: { name: 'A', email: 'a@x' },
            time: '2026-01-02T00:00:00Z',
            message: 'Update',
            change: 'update',
            version: V1,
          },
        ],
      },
    },
    [`POST ${S}/restore/notes/plan.md`]: () =>
      new Promise((done) => {
        restored = () => done({ body: { commit: 'c4', doc: plan } })
      }),
  })
  beginSession(1)
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  await within(doc).findByRole('button', { name: 'Edit' })
  fromMenu(doc, 'Delete')
  fireEvent.click(
    within(screen.getByRole('alertdialog', { name: 'Delete the document' })).getByRole('button', { name: 'Delete' }),
  )
  fireEvent.click(await screen.findByRole('button', { name: 'Restore' }))
  await waitFor(() => expect(calls.some((c) => c.path.startsWith(`${S}/restore/`))).toBe(true))
  endSession()
  await act(async () => restored())
  await new Promise((r) => setTimeout(r, 20))
  expect(screen.queryByRole('article', { name: 'notes/plan.md' })).toBeNull()
})

test('an upload answered after its session ended opens nothing, though the form is still here', async () => {
  let stored: () => void = () => {}
  kb('write', {
    [`PUT ${S}/docs/logo.png`]: () =>
      new Promise((done) => {
        stored = () => done({ status: 201, body: { commit: 'c1', doc: { path: 'logo.png', version: V2, size: 3 } } })
      }),
    [`GET ${S}/docs/logo.png`]: { body: { path: 'logo.png', version: V2, size: 3, content_base64: 'AQID' } },
  })
  beginSession(1)
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Upload' }))
  const dialog = screen.getByRole('dialog', { name: 'Upload a file' })
  fireEvent.change(within(dialog).getByLabelText('File'), {
    target: { files: [new File([new Uint8Array([1, 2, 3])], 'logo.png')] },
  })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Upload' }))
  await new Promise((r) => setTimeout(r, 20))
  endSession()
  await act(async () => stored())
  await new Promise((r) => setTimeout(r, 20))
  expect(screen.queryByRole('article', { name: 'logo.png' })).toBeNull()
})
