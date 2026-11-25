// @vitest-environment jsdom

// Answers that arrive late, and drafts that must not be lost: each test holds a response open
// (or edits while one is on its way) and checks that nothing from the past is shown or done.
// The fake fetch ends an aborted request as a real one does, so an abort is exercised too.

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { type Call, fakeFetch } from '../test/fakes'
import { KnowledgePage } from './KnowledgePage'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  window.history.pushState(null, '', '/')
})

const S = '/api/workspaces/1/spaces/3'
const S4 = '/api/workspaces/1/spaces/4'
// The list of a space's documents, as it is asked for.
const LIST = `${S}/docs?limit=1000`
const LIST4 = `${S4}/docs?limit=1000`
const V1 = 'a'.repeat(40)
const V2 = 'b'.repeat(40)
const space = (id: number, name: string) => ({
  id,
  workspace_id: 1,
  kind: 'shared',
  name,
  archived_at: null,
  role: 'editor',
  access: 'write',
})
const plan = { path: 'notes/plan.md', version: V1, size: 7, versioned: true }
const other = { path: 'notes/other.md', version: V2, size: 5, versioned: true }
const commit = (id: string, change: 'create' | 'update' | 'delete', extra: Record<string, unknown> = {}) => ({
  commit: id,
  author: { name: 'A', email: 'a@x' },
  time: '2026-01-02T00:00:00Z',
  message: id,
  change,
  ...extra,
})

// A response held until the test lets it go.
function gate<T>() {
  let release!: (value: T) => void
  const answer = new Promise<T>((resolve) => {
    release = resolve
  })
  return { answer, release }
}

// The app has read a late answer, and what follows from it has run: so a check that nothing came
// of it is a check of an answer that was delivered, not of one still on its way.
async function delivered(calls: Call[], method: string, path: string) {
  const call = calls.find((c) => c.method === method && c.path === path)
  expect(call, `${method} ${path} was never made`).toBeDefined()
  await act(async () => {
    await call?.read
    await new Promise<void>((resolve) => setTimeout(resolve, 0))
  })
}
const reads = (calls: Call[], path: string) => calls.filter((c) => c.method === 'GET' && c.path === path).length
const made = (calls: Call[], method: string, path?: string) =>
  calls.some((c) => c.method === method && (path === undefined || c.path === path))

function kb(extra: Parameters<typeof fakeFetch>[0] = {}) {
  return fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces: [space(3, 'Team')] } },
    [`GET ${S}/docs`]: { body: { docs: [plan, other] } },
    [`GET ${S}/docs/notes/plan.md`]: { body: { ...plan, content: '# Plan\n' } },
    [`GET ${S}/docs/notes/other.md`]: { body: { ...other, content: '# Other\n' } },
    ...extra,
  })
}

async function open(name = 'notes/plan.md') {
  fireEvent.click(await screen.findByRole('button', { name: new RegExp(name.replace('.', '\\.')) }))
  return screen.findByRole('article', { name })
}
const inList = (name: RegExp) => screen.getByRole('button', { name })

// The delete question of a document, answered yes.
async function confirmDelete(doc: HTMLElement) {
  fireEvent.click(await within(doc).findByRole('button', { name: 'Delete' }))
  fireEvent.click(
    within(screen.getByRole('group', { name: 'Delete the document' })).getByRole('button', { name: 'Delete' }),
  )
}

// A draft in the editor of the open document.
async function draft(doc: HTMLElement, text = 'Mine.') {
  fireEvent.click(await within(doc).findByRole('button', { name: 'Edit' }))
  fireEvent.change(await within(doc).findByLabelText('Text'), { target: { value: text } })
  return within(doc).getByLabelText('Text') as HTMLTextAreaElement
}
const asked = () => screen.queryByRole('alertdialog', { name: 'Discard your changes' })

test('the fake ends an aborted request as a fetch does', async () => {
  fakeFetch({ 'GET /x': () => new Promise(() => {}) })
  const stop = new AbortController()
  const request = fetch('/x', { signal: stop.signal })
  stop.abort()
  await expect(request).rejects.toMatchObject({ name: 'AbortError' })
  await expect(fetch('/x', { signal: stop.signal })).rejects.toMatchObject({ name: 'AbortError' })
})

test('a delete that finishes in another space leaves that space alone', async () => {
  const held = gate<{ body: unknown }>()
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces: [space(3, 'Team'), space(4, 'Other')] } },
    [`GET ${S}/docs`]: { body: { docs: [plan] } },
    [`GET ${S}/docs/notes/plan.md`]: { body: { ...plan, content: '# Plan\n' } },
    [`DELETE ${S}/docs/notes/plan.md`]: () => held.answer,
    // The other space has a file of the same name.
    [`GET ${S4}/docs`]: { body: { docs: [plan] } },
    [`GET ${S4}/docs/notes/plan.md`]: { body: { ...plan, content: 'The other one.' } },
    [`GET ${S4}/history/notes/plan.md`]: { body: { commits: [commit('x2', 'update', { version: V1 })] } },
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  await confirmDelete(doc)
  await waitFor(() => expect(made(calls, 'DELETE')).toBe(true))

  // The person goes to the other space and opens its file of that name; then the delete ends.
  fireEvent.change(await screen.findByLabelText('Space'), { target: { value: '4' } })
  const there = await open()
  await within(there).findByText('The other one.')
  held.release({ body: { commit: 'c3' } })
  await delivered(calls, 'DELETE', `${S}/docs/notes/plan.md`)

  // No notice, so no Restore that could act on this space's file; its document stays open.
  expect(screen.queryByText(/Deleted notes\/plan\.md/)).toBeNull()
  expect(screen.queryByRole('button', { name: 'Restore' })).toBeNull()
  expect(screen.getByRole('article', { name: 'notes/plan.md' })).toBeTruthy()
  expect(made(calls, 'POST')).toBe(false)
})

test('a delete that finishes beside another open document of its space keeps that one open', async () => {
  const held = gate<{ body: unknown }>()
  const calls = kb({ [`DELETE ${S}/docs/notes/plan.md`]: () => held.answer })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  await confirmDelete(doc)
  fireEvent.click(inList(/notes\/other\.md/))
  await screen.findByRole('article', { name: 'notes/other.md' })
  held.release({ body: { commit: 'c3' } })
  // The notice names the document it was made for, and the one being read is still there.
  await screen.findByText(/Deleted notes\/plan\.md/)
  await delivered(calls, 'DELETE', `${S}/docs/notes/plan.md`)
  expect(screen.getByRole('article', { name: 'notes/other.md' })).toBeTruthy()
})

test('a restore that finishes after Edit was opened keeps the draft', async () => {
  const held = gate<{ body: unknown }>()
  const calls = kb({
    [`GET ${S}/history/notes/plan.md`]: {
      body: { commits: [commit('c2', 'update', { version: V1 }), commit('c1', 'create', { version: V2 })] },
    },
    [`POST ${S}/restore/notes/plan.md`]: () => held.answer,
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  fireEvent.click(await within(doc).findByRole('button', { name: 'Restore this version' }))
  await waitFor(() => expect(made(calls, 'POST')).toBe(true))

  fireEvent.click(within(doc).getByRole('button', { name: 'Edit' }))
  fireEvent.change(await within(doc).findByLabelText('Text'), { target: { value: 'My new draft.' } })
  held.release({ body: { commit: 'c3', doc: plan } })
  await delivered(calls, 'POST', `${S}/restore/notes/plan.md`)

  expect((within(doc).getByLabelText('Text') as HTMLTextAreaElement).value).toBe('My new draft.')
})

test('Cancel, Back and opening another document ask before a changed draft is thrown away', async () => {
  kb()
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  const text = await draft(doc)

  // Cancel asks; keeping the draft leaves it as it was.
  fireEvent.click(within(doc).getByRole('button', { name: 'Cancel' }))
  fireEvent.click(within(within(doc).getByRole('alertdialog')).getByRole('button', { name: 'Keep editing' }))
  expect(asked()).toBeNull()
  expect(text.value).toBe('Mine.')

  // Back asks too, and so does opening another document from the list.
  fireEvent.click(within(doc).getByRole('button', { name: 'Back to the documents' }))
  expect(asked()).toBeTruthy()
  fireEvent.click(within(doc).getByRole('button', { name: 'Keep editing' }))
  fireEvent.click(inList(/notes\/other\.md/))
  expect(asked()).toBeTruthy()
  expect(screen.getByRole('article', { name: 'notes/plan.md' })).toBeTruthy()
  expect(text.value).toBe('Mine.')

  // Saying yes does what was asked.
  fireEvent.click(within(doc).getByRole('button', { name: 'Discard my changes' }))
  await screen.findByRole('article', { name: 'notes/other.md' })
  expect(screen.queryByRole('article', { name: 'notes/plan.md' })).toBeNull()
})

test('a commit message alone is a change that is asked about', async () => {
  kb()
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'Edit' }))
  fireEvent.change(within(doc).getByLabelText('What changed (optional)'), { target: { value: 'Tidy' } })
  fireEvent.click(within(doc).getByRole('button', { name: 'Cancel' }))
  expect(asked()).toBeTruthy()
})

test('Cancel and Back go at once when the draft has not changed', async () => {
  kb()
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'Edit' }))
  // Typing and undoing it is no change.
  const text = within(doc).getByLabelText('Text')
  fireEvent.change(text, { target: { value: 'x' } })
  fireEvent.change(text, { target: { value: '# Plan\n' } })
  fireEvent.click(within(doc).getByRole('button', { name: 'Cancel' }))
  expect(asked()).toBeNull()
  expect(within(doc).queryByLabelText('Text')).toBeNull()

  fireEvent.click(within(doc).getByRole('button', { name: 'Edit' }))
  fireEvent.click(within(doc).getByRole('button', { name: 'Back to the documents' }))
  expect(asked()).toBeNull()
  expect(screen.queryByRole('article')).toBeNull()
})

test('the space picker, a search result and a form that opens its document each ask first', async () => {
  const hit = {
    space_id: 3,
    space_name: 'Team',
    path: 'notes/other.md',
    title: 'Other note',
    snippet: [{ text: 'other' }],
    score: 0.5,
    match: 'both',
    version: V2,
    commit: 'c'.repeat(40),
  }
  const none = { body: { results: [] } }
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces: [space(3, 'Team'), space(4, 'Other')] } },
    [`GET ${S}/docs`]: { body: { docs: [plan, other] } },
    [`GET ${S}/docs/notes/plan.md`]: { body: { ...plan, content: '# Plan\n' } },
    [`GET ${S}/docs/notes/other.md`]: { body: { ...other, content: '# Other\n' } },
    [`GET ${S}/docs/fresh.md`]: { body: { path: 'fresh.md', version: V2, size: 9, content: '# fresh\n' } },
    [`PUT ${S}/docs/fresh.md`]: {
      status: 201,
      body: { commit: 'c1', doc: { path: 'fresh.md', version: V2, size: 9 } },
    },
    [`GET ${S4}/docs`]: { body: { docs: [] } },
    [`GET ${S}/search`]: { body: { results: [hit], mode: 'hybrid' } },
    [`GET ${S}/search/cards`]: none,
    [`GET ${S}/search/records`]: none,
    'GET /api/workspaces/1/search/conversations': none,
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  const text = await draft(doc)

  // The space picker.
  fireEvent.change(screen.getByLabelText('Space'), { target: { value: '4' } })
  expect(asked()).toBeTruthy()
  expect(text.value).toBe('Mine.')
  fireEvent.click(within(doc).getByRole('button', { name: 'Keep editing' }))
  expect(screen.getByRole('article', { name: 'notes/plan.md' })).toBeTruthy()

  // A search result.
  fireEvent.change(screen.getByLabelText('Search the knowledge base'), { target: { value: 'other' } })
  fireEvent.click(screen.getByRole('button', { name: 'Search' }))
  fireEvent.click(await screen.findByRole('button', { name: /Other note/ }))
  expect(asked()).toBeTruthy()
  expect(screen.getByRole('article', { name: 'notes/plan.md' })).toBeTruthy()
  fireEvent.click(within(doc).getByRole('button', { name: 'Keep editing' }))

  // A new document is made and the list read again, but it opens only when the draft is let go.
  fireEvent.click(screen.getByRole('button', { name: 'New' }))
  fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'fresh' } })
  fireEvent.click(screen.getByRole('button', { name: 'Create' }))
  await within(doc).findByRole('alertdialog', { name: 'Discard your changes' })
  expect(reads(calls, LIST)).toBe(2)
  expect(screen.getByRole('article', { name: 'notes/plan.md' })).toBeTruthy()
  expect(text.value).toBe('Mine.')
  fireEvent.click(within(doc).getByRole('button', { name: 'Discard my changes' }))
  await screen.findByRole('article', { name: 'fresh.md' })
})

test('the question is in view, has the focus, and Keep editing (or Escape) gives it back to the text', async () => {
  const scrolled = vi.fn()
  Object.defineProperty(Element.prototype, 'scrollIntoView', { value: scrolled, configurable: true })
  try {
    kb()
    render(<KnowledgePage workspace={1} />)
    const doc = await open()
    const text = await draft(doc, 'Some words here.')
    text.setSelectionRange(5, 9)

    fireEvent.click(within(doc).getByRole('button', { name: 'Cancel' }))
    const question = within(doc).getByRole('alertdialog', { name: 'Discard your changes' })
    expect(scrolled).toHaveBeenCalled()
    expect(document.activeElement).toBe(within(question).getByRole('button', { name: 'Keep editing' }))

    fireEvent.click(within(question).getByRole('button', { name: 'Keep editing' }))
    expect(asked()).toBeNull()
    expect(document.activeElement).toBe(text)
    expect([text.selectionStart, text.selectionEnd]).toEqual([5, 9])

    // Escape means the same.
    text.setSelectionRange(2, 2)
    fireEvent.click(inList(/notes\/other\.md/))
    expect(asked()).toBeTruthy()
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Keep editing' }))
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(asked()).toBeNull()
    expect(document.activeElement).toBe(text)
    expect([text.selectionStart, text.selectionEnd]).toEqual([2, 2])
    expect(screen.getByRole('article', { name: 'notes/plan.md' })).toBeTruthy()
  } finally {
    delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView
  }
})

test('a save in flight is waited for, not asked about; one that fails then asks', async () => {
  const held = gate<{ status?: number; body: unknown }>()
  const calls = kb({ [`PUT ${S}/docs/notes/plan.md`]: () => held.answer })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  await draft(doc)
  fireEvent.click(within(doc).getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(made(calls, 'PUT')).toBe(true))

  // Going elsewhere while it saves asks nothing and waits: the text is being kept.
  fireEvent.click(inList(/notes\/other\.md/))
  expect(asked()).toBeNull()
  expect(screen.getByRole('article', { name: 'notes/plan.md' })).toBeTruthy()
  held.release({ body: { commit: 'c3', doc: { ...plan, version: V2 } } })
  await screen.findByRole('article', { name: 'notes/other.md' })
  expect(asked()).toBeNull()
})

test('Discard is not offered while a save lands anyway: a save that fails asks then', async () => {
  const held = gate<{ status?: number; body: unknown }>()
  const calls = kb({ [`PUT ${S}/docs/notes/plan.md`]: () => held.answer })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  const text = await draft(doc)
  fireEvent.click(within(doc).getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(made(calls, 'PUT')).toBe(true))
  fireEvent.click(inList(/notes\/other\.md/))
  held.release({ status: 409, body: { error: 'conflict' } })

  // The draft is still there, so now it is asked, and nothing was discarded meanwhile.
  await within(doc).findByRole('alertdialog', { name: 'Discard your changes' })
  expect(text.value).toBe('Mine.')
  expect(screen.getByRole('article', { name: 'notes/plan.md' })).toBeTruthy()
})

test('a question asked after the person saves is dropped, and what they were doing goes on after the save', async () => {
  const held = gate<{ status?: number; body: unknown }>()
  const calls = kb({ [`PUT ${S}/docs/notes/plan.md`]: () => held.answer })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  await draft(doc)
  fireEvent.click(inList(/notes\/other\.md/))
  expect(asked()).toBeTruthy()
  // Saving instead of answering is an answer: the question goes, and the save is not undone by one.
  fireEvent.click(within(doc).getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(made(calls, 'PUT')).toBe(true))
  expect(asked()).toBeNull()
  held.release({ body: { commit: 'c3', doc: { ...plan, version: V2 } } })
  await screen.findByRole('article', { name: 'notes/other.md' })
})

test('leaving the page asks through the browser, and a link of the app asks in the page', async () => {
  kb()
  render(
    <>
      <a href="/w/1/boards">Boards</a>
      <KnowledgePage workspace={1} />
    </>,
  )
  const doc = await open()
  const unload = () => {
    const e = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(e)
    return e.defaultPrevented
  }
  // Nothing changed: nothing is asked.
  fireEvent.click(await within(doc).findByRole('button', { name: 'Edit' }))
  expect(unload()).toBe(false)
  fireEvent.change(within(doc).getByLabelText('Text'), { target: { value: 'Mine.' } })
  expect(unload()).toBe(true)

  fireEvent.click(screen.getByRole('link', { name: 'Boards' }))
  expect(asked()).toBeTruthy()
  expect(window.location.pathname).toBe('/')
  fireEvent.click(within(doc).getByRole('button', { name: 'Discard my changes' }))
  expect(window.location.pathname).toBe('/w/1/boards')
})

test('a delete that lands while the document is being edited says why it asks', async () => {
  const held = gate<{ body: unknown }>()
  const calls = kb({ [`DELETE ${S}/docs/notes/plan.md`]: () => held.answer })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  await confirmDelete(doc)
  // Cancel doesn't stop the delete; the person goes on to edit the document.
  fireEvent.click(within(doc).getByRole('button', { name: 'Cancel' }))
  const text = await draft(doc)
  held.release({ body: { commit: 'c3' } })
  const question = await within(doc).findByRole('alertdialog', { name: 'Discard your changes' })
  await delivered(calls, 'DELETE', `${S}/docs/notes/plan.md`)
  expect(question.textContent).toContain('This document was deleted.')
  expect(text.value).toBe('Mine.')
})

test('a move that lands after Edit was opened asks, saying the document was moved', async () => {
  const held = gate<{ body: unknown }>()
  const calls = kb({
    [`PATCH ${S}/docs/notes/plan.md`]: () => held.answer,
    [`GET ${S}/docs/archive/plan.md`]: { body: { ...plan, path: 'archive/plan.md', content: 'moved' } },
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'Move' }))
  fireEvent.change(within(doc).getByLabelText('New name'), { target: { value: 'archive/plan.md' } })
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Move' }).at(-1) as HTMLElement)
  await waitFor(() => expect(made(calls, 'PATCH')).toBe(true))
  const text = await draft(doc)
  held.release({ body: { commit: 'c2', doc: { ...plan, path: 'archive/plan.md' } } })

  const question = await within(doc).findByRole('alertdialog', { name: 'Discard your changes' })
  expect(question.textContent).toContain('This document was moved.')
  expect(text.value).toBe('Mine.')
  expect(screen.queryByRole('article', { name: 'archive/plan.md' })).toBeNull()
  fireEvent.click(within(doc).getByRole('button', { name: 'Discard my changes' }))
  await screen.findByRole('article', { name: 'archive/plan.md' })
})

test('a move that lands after the person went to another document opens nothing', async () => {
  const held = gate<{ body: unknown }>()
  const calls = kb({ [`PATCH ${S}/docs/notes/plan.md`]: () => held.answer })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'Move' }))
  fireEvent.change(within(doc).getByLabelText('New name'), { target: { value: 'archive/plan.md' } })
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Move' }).at(-1) as HTMLElement)
  await waitFor(() => expect(made(calls, 'PATCH')).toBe(true))
  fireEvent.click(inList(/notes\/other\.md/))
  await screen.findByRole('article', { name: 'notes/other.md' })
  held.release({ body: { commit: 'c2', doc: { ...plan, path: 'archive/plan.md' } } })
  await delivered(calls, 'PATCH', `${S}/docs/notes/plan.md`)
  expect(screen.getByRole('article', { name: 'notes/other.md' })).toBeTruthy()
  expect(reads(calls, LIST)).toBe(2)
})

// Starts an upload of logo.png whose request is held, and returns it.
async function startUpload(calls: Call[]) {
  fireEvent.click(await screen.findByRole('button', { name: 'Upload' }))
  const form = screen.getByRole('form', { name: 'Upload a file' })
  fireEvent.change(within(form).getByLabelText('File'), {
    target: { files: [new File([new Uint8Array([1, 2, 3])], 'logo.png')] },
  })
  fireEvent.click(within(form).getByRole('button', { name: 'Upload' }))
  await waitFor(() => expect(made(calls, 'PUT')).toBe(true))
  return { form, put: calls.find((c) => c.method === 'PUT') as Call }
}
const uploaded = { status: 201, body: { commit: 'c1', doc: { path: 'logo.png', version: V2, size: 3 } } }

test('Cancel stops an upload, reads the list again, and nothing opens', async () => {
  const held = gate<typeof uploaded>()
  const calls = kb({
    [`PUT ${S}/docs/logo.png`]: () => held.answer,
    [`GET ${S}/docs/logo.png`]: { body: { path: 'logo.png', version: V2, size: 3, content_base64: 'AQID' } },
  })
  render(<KnowledgePage workspace={1} />)
  const { form, put } = await startUpload(calls)
  expect(reads(calls, LIST)).toBe(1)

  fireEvent.click(within(form).getByRole('button', { name: 'Cancel' }))
  expect(put.signal?.aborted).toBe(true)
  // The server may have finished all the same: the list says what is there.
  await waitFor(() => expect(reads(calls, LIST)).toBe(2))
  expect(screen.queryByRole('article')).toBeNull()
  expect(screen.queryByRole('alert')).toBeNull()
})

test('an upload goes on when its form goes away: it finishes, the list is read again, and nothing opens', async () => {
  const held = gate<typeof uploaded>()
  const calls = kb({
    [`PUT ${S}/docs/logo.png`]: () => held.answer,
    [`GET ${S}/docs/logo.png`]: { body: { path: 'logo.png', version: V2, size: 3, content_base64: 'AQID' } },
  })
  render(<KnowledgePage workspace={1} />)
  const { put } = await startUpload(calls)

  // The person opens a document of the list: the form closes, and the upload is not stopped.
  fireEvent.click(inList(/notes\/other\.md/))
  await screen.findByRole('article', { name: 'notes/other.md' })
  expect(screen.queryByRole('form', { name: 'Upload a file' })).toBeNull()
  expect(put.signal?.aborted).toBe(false)
  held.release(uploaded)
  await delivered(calls, 'PUT', `${S}/docs/logo.png`)
  expect(reads(calls, LIST)).toBe(2)
  expect(screen.getByRole('article', { name: 'notes/other.md' })).toBeTruthy()
  expect(screen.queryByRole('article', { name: 'logo.png' })).toBeNull()
})

test('an upload goes on when the person opens New, or goes to another space', async () => {
  const held = gate<typeof uploaded>()
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces: [space(3, 'Team'), space(4, 'Other')] } },
    [`GET ${S}/docs`]: { body: { docs: [plan] } },
    [`GET ${S4}/docs`]: { body: { docs: [] } },
    [`PUT ${S}/docs/logo.png`]: () => held.answer,
  })
  render(<KnowledgePage workspace={1} />)
  const { put } = await startUpload(calls)
  fireEvent.click(screen.getByRole('button', { name: 'New' }))
  expect(screen.queryByRole('form', { name: 'Upload a file' })).toBeNull()
  expect(put.signal?.aborted).toBe(false)

  fireEvent.change(screen.getByLabelText('Space'), { target: { value: '4' } })
  await waitFor(() => expect(reads(calls, LIST4)).toBe(1))
  held.release(uploaded)
  await delivered(calls, 'PUT', `${S}/docs/logo.png`)
  // The other space's list is not the one that changed; nothing opened in it.
  expect(reads(calls, LIST4)).toBe(1)
  expect(screen.queryByRole('article')).toBeNull()
})

test('removing old versions while an older page loads shows no error, and Older works on the new list', async () => {
  const held = gate<{ body: unknown }>()
  let removed = false
  const entry = (id: string) => commit(id, 'update', { version: V1 })
  kb({
    [`GET ${S}/history/notes/plan.md`]: () =>
      removed ? { body: { commits: [entry('n3')], next: 'n3' } } : { body: { commits: [entry('c3')], next: 'c3' } },
    [`GET ${S}/history/notes/plan.md?before=c3`]: () => held.answer,
    [`GET ${S}/history/notes/plan.md?before=n3`]: { body: { commits: [entry('n2')] } },
    [`DELETE ${S}/history/notes/plan.md`]: () => {
      removed = true
      return { body: { commit: 'n4', doc: plan, removed: 1 } }
    },
  })
  render(<KnowledgePage workspace={1} admin />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  fireEvent.click(await within(doc).findByRole('button', { name: 'Older' }))
  fireEvent.click(within(doc).getByRole('button', { name: 'Remove old versions from history…' }))
  fireEvent.click(within(doc).getByRole('button', { name: 'Remove old versions' }))
  await within(doc).findByText('n3')

  // The page that was loading was ended by the list being made again: not a failure to show.
  expect(within(doc).queryByRole('alert')).toBeNull()
  // And the new list loads its own older page: nothing says one is still loading.
  fireEvent.click(await within(doc).findByRole('button', { name: 'Older' }))
  await within(doc).findByText('n2')
  expect(within(doc).queryByRole('alert')).toBeNull()
  expect(within(doc).queryByText('c3')).toBeNull()
})

test('the history is not offered for viewing while it loads again after a removal', async () => {
  const held = gate<{ body: unknown }>()
  let removed = false
  const entry = (id: string) => commit(id, 'update', { version: V2 })
  kb({
    [`GET ${S}/history/notes/plan.md`]: () =>
      removed ? held.answer : { body: { commits: [commit('c3', 'update', { version: V1 }), entry('c2')] } },
    [`DELETE ${S}/history/notes/plan.md`]: () => {
      removed = true
      return { body: { commit: 'n4', doc: plan, removed: 1 } }
    },
  })
  render(<KnowledgePage workspace={1} admin />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  await within(doc).findAllByRole('button', { name: 'View' })
  fireEvent.click(within(doc).getByRole('button', { name: 'Remove old versions from history…' }))
  fireEvent.click(within(doc).getByRole('button', { name: 'Remove old versions' }))
  await within(doc).findByText('Removed 1 old version from the history.')

  // c2's commit no longer exists: until the new list is here, there is nothing to click.
  await waitFor(() => expect(within(doc).queryByRole('button', { name: 'View' })).toBeNull())
  held.release({ body: { commits: [commit('n3', 'update', { version: V1 })] } })
  await within(doc).findByText('n3')
  expect(within(doc).getAllByRole('button', { name: 'View' })).toHaveLength(1)
})

// The server answers a history page that starts after a commit the history no longer has with a
// 404 (and a request made on a document that changed meanwhile with a 409): Reload is offered
// for either, and loads the history again, not the document.
test.each([404, 409])(
  'when older history fails with a %i, Reload loads the history again, not the document',
  async (status) => {
    let failing = true
    const calls = kb({
      [`GET ${S}/history/notes/plan.md`]: { body: { commits: [commit('c3', 'update', { version: V1 })], next: 'c3' } },
      [`GET ${S}/history/notes/plan.md?before=c3`]: () =>
        failing ? { status, body: { error: 'gone' } } : { body: { commits: [] } },
    })
    render(<KnowledgePage workspace={1} />)
    const doc = await open()
    fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
    fireEvent.click(await within(doc).findByRole('button', { name: 'Older' }))
    const alert = await within(doc).findByRole('alert')
    expect(reads(calls, `${S}/history/notes/plan.md`)).toBe(1)

    failing = false
    fireEvent.click(within(alert).getByRole('button', { name: 'Reload' }))
    await waitFor(() => expect(reads(calls, `${S}/history/notes/plan.md`)).toBe(2))
    expect(reads(calls, `${S}/docs/notes/plan.md`)).toBe(1)
    expect(within(doc).queryByRole('alert')).toBeNull()
  },
)

test('an older page that fails some other way has no Reload to offer', async () => {
  kb({
    [`GET ${S}/history/notes/plan.md`]: { body: { commits: [commit('c3', 'update', { version: V1 })], next: 'c3' } },
    [`GET ${S}/history/notes/plan.md?before=c3`]: { status: 500, body: { error: 'disk on fire' } },
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  fireEvent.click(await within(doc).findByRole('button', { name: 'Older' }))
  const alert = await within(doc).findByRole('alert')
  expect(alert.textContent).toContain('disk on fire')
  expect(within(alert).queryByRole('button', { name: 'Reload' })).toBeNull()
})

test('after a restore conflict, Reload loads the document and its history', async () => {
  const calls = kb({
    [`GET ${S}/history/notes/plan.md`]: {
      body: { commits: [commit('c2', 'update', { version: V1 }), commit('c1', 'create', { version: V2 })] },
    },
    [`POST ${S}/restore/notes/plan.md`]: { status: 409, body: { error: 'conflict' } },
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  fireEvent.click(await within(doc).findByRole('button', { name: 'Restore this version' }))
  const alert = await within(doc).findByRole('alert')
  fireEvent.click(within(alert).getByRole('button', { name: 'Reload' }))
  await waitFor(() => expect(reads(calls, `${S}/history/notes/plan.md`)).toBe(2))
  await waitFor(() => expect(reads(calls, `${S}/docs/notes/plan.md`)).toBe(2))
})

// A deleted document's notice, with its Restore button, after `history` was answered.
async function deletedNotice(extra: Parameters<typeof fakeFetch>[0]) {
  const calls = kb({ [`DELETE ${S}/docs/notes/plan.md`]: { body: { commit: 'c30' } }, ...extra })
  render(<KnowledgePage workspace={1} />)
  await confirmDelete(await open())
  return { calls, restore: await screen.findByRole('button', { name: 'Restore' }) }
}

test('restoring a deleted document finds the version however far back it is', async () => {
  const kept = (id: string) => commit(id, 'update', { version: V1 })
  const removed = (id: string) => commit(id, 'update', { version: V2, versioned: false })
  const { calls, restore } = await deletedNotice({
    // The newest entries are the delete and a run of versions that were removed from history.
    [`GET ${S}/history/notes/plan.md`]: {
      body: {
        commits: [commit('c30', 'delete'), ...Array.from({ length: 12 }, (_, i) => removed(`r${i}`))],
        next: 'r11',
      },
    },
    [`GET ${S}/history/notes/plan.md?before=r11`]: { body: { commits: [removed('r12'), kept('c1')] } },
    [`POST ${S}/restore/notes/plan.md`]: { body: { commit: 'c31', doc: plan } },
  })
  fireEvent.click(restore)
  await screen.findByRole('article', { name: 'notes/plan.md' })
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ commit: 'c1' })
})

test('restoring a deleted document says so when no version of it is left, and sends nothing', async () => {
  const { calls, restore } = await deletedNotice({
    [`GET ${S}/history/notes/plan.md`]: {
      body: { commits: [commit('c30', 'delete'), commit('r1', 'update', { version: V2, versioned: false })] },
    },
  })
  fireEvent.click(restore)
  await screen.findByText('No earlier version of notes/plan.md was found.')
  expect(made(calls, 'POST')).toBe(false)
  expect(reads(calls, `${S}/history/notes/plan.md`)).toBe(1)
})

test('a restore that finishes after the notice was left reads the list again and opens nothing', async () => {
  const held = gate<{ body: unknown }>()
  const { calls, restore } = await deletedNotice({
    [`GET ${S}/history/notes/plan.md`]: {
      body: { commits: [commit('c30', 'delete'), commit('c2', 'update', { version: V1 })] },
    },
    [`POST ${S}/restore/notes/plan.md`]: () => held.answer,
  })
  fireEvent.click(restore)
  await waitFor(() => expect(made(calls, 'POST')).toBe(true))
  expect(reads(calls, LIST)).toBe(2)

  // The person opens another document: the notice goes, and the restore ends after that.
  fireEvent.click(inList(/notes\/other\.md/))
  await screen.findByRole('article', { name: 'notes/other.md' })
  held.release({ body: { commit: 'c31', doc: plan } })
  await delivered(calls, 'POST', `${S}/restore/notes/plan.md`)
  expect(screen.getByRole('article', { name: 'notes/other.md' })).toBeTruthy()
  expect(screen.queryByRole('article', { name: 'notes/plan.md' })).toBeNull()
  // The restored document is in the list, though it was not opened.
  expect(reads(calls, LIST)).toBe(3)
})

test('a restore whose history is still being searched is sent all the same when the notice goes', async () => {
  const held = gate<{ body: unknown }>()
  const { calls, restore } = await deletedNotice({
    [`GET ${S}/history/notes/plan.md`]: () => held.answer,
    [`POST ${S}/restore/notes/plan.md`]: { body: { commit: 'c31', doc: plan } },
  })
  fireEvent.click(restore)
  await waitFor(() => expect(made(calls, 'GET', `${S}/history/notes/plan.md`)).toBe(true))
  fireEvent.click(inList(/notes\/other\.md/))
  await screen.findByRole('article', { name: 'notes/other.md' })

  held.release({ body: { commits: [commit('c30', 'delete'), commit('c2', 'update', { version: V1 })] } })
  await waitFor(() => expect(made(calls, 'POST')).toBe(true))
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ commit: 'c2' })
  await delivered(calls, 'POST', `${S}/restore/notes/plan.md`)
  // Opened nothing, but the list has the document again.
  expect(screen.getByRole('article', { name: 'notes/other.md' })).toBeTruthy()
  expect(reads(calls, LIST)).toBe(3)
})
