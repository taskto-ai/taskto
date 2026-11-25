// @vitest-environment jsdom

// Answers that arrive late, and drafts that must not be lost: each test holds a response open
// (or edits while one is on its way) and checks that nothing from the past is shown or done.

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { fakeFetch } from '../test/fakes'
import { KnowledgePage } from './KnowledgePage'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const S = '/api/workspaces/1/spaces/3'
const S4 = '/api/workspaces/1/spaces/4'
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
const flush = () => act(() => new Promise<void>((resolve) => setTimeout(resolve, 30)))

function kb(extra: Parameters<typeof fakeFetch>[0] = {}) {
  return fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces: [space(3, 'Team')] } },
    [`GET ${S}/docs`]: { body: { docs: [plan, other] } },
    [`GET ${S}/docs/notes/plan.md`]: { body: { ...plan, content: '# Plan\n' } },
    [`GET ${S}/docs/notes/other.md`]: { body: { ...other, content: '# Other\n' } },
    ...extra,
  })
}

async function open(name: RegExp | string = /notes\/plan\.md/) {
  fireEvent.click(await screen.findByRole('button', { name }))
  return screen.findByRole('article', { name: typeof name === 'string' ? name : 'notes/plan.md' })
}

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
  fireEvent.click(await within(doc).findByRole('button', { name: 'Delete' }))
  fireEvent.click(
    within(screen.getByRole('group', { name: 'Delete the document' })).getByRole('button', { name: 'Delete' }),
  )
  await waitFor(() => expect(calls.some((c) => c.method === 'DELETE')).toBe(true))

  // The person goes to the other space and opens its file of that name; then the delete ends.
  fireEvent.change(await screen.findByLabelText('Space'), { target: { value: '4' } })
  const there = await open()
  await within(there).findByText('The other one.')
  held.release({ body: { commit: 'c3' } })
  await flush()

  // No notice, so no Restore that could act on this space's file; its document stays open.
  expect(screen.queryByText(/Deleted notes\/plan\.md/)).toBeNull()
  expect(screen.queryByRole('button', { name: 'Restore' })).toBeNull()
  expect(screen.getByRole('article', { name: 'notes/plan.md' })).toBeTruthy()
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)
})

test('a delete that finishes beside another open document of its space keeps that one open', async () => {
  const held = gate<{ body: unknown }>()
  kb({
    [`DELETE ${S}/docs/notes/plan.md`]: () => held.answer,
    [`GET ${S}/history/notes/plan.md`]: {
      body: { commits: [commit('c3', 'delete'), commit('c2', 'update', { version: V1 })] },
    },
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'Delete' }))
  fireEvent.click(
    within(screen.getByRole('group', { name: 'Delete the document' })).getByRole('button', { name: 'Delete' }),
  )
  fireEvent.click(screen.getByRole('button', { name: /notes\/other\.md/ }))
  await screen.findByRole('article', { name: 'notes/other.md' })
  held.release({ body: { commit: 'c3' } })
  // The notice names the document it was made for, and the one being read is still there.
  await screen.findByText(/Deleted notes\/plan\.md/)
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
  await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))

  fireEvent.click(within(doc).getByRole('button', { name: 'Edit' }))
  fireEvent.change(await within(doc).findByLabelText('Text'), { target: { value: 'My new draft.' } })
  held.release({ body: { commit: 'c3', doc: plan } })
  await flush()

  expect((within(doc).getByLabelText('Text') as HTMLTextAreaElement).value).toBe('My new draft.')
})

test('Cancel, Back and opening another document ask before a changed draft is thrown away', async () => {
  kb()
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'Edit' }))
  const text = () => within(doc).getByLabelText('Text') as HTMLTextAreaElement
  fireEvent.change(text(), { target: { value: 'Mine.' } })

  // Cancel asks; keeping the draft leaves it as it was.
  fireEvent.click(within(doc).getByRole('button', { name: 'Cancel' }))
  const question = within(doc).getByRole('group', { name: 'Discard your changes' })
  fireEvent.click(within(question).getByRole('button', { name: 'Keep editing' }))
  expect(within(doc).queryByRole('group', { name: 'Discard your changes' })).toBeNull()
  expect(text().value).toBe('Mine.')

  // Back asks too, and so does opening another document from the list.
  fireEvent.click(within(doc).getByRole('button', { name: 'Back to the documents' }))
  expect(within(doc).getByRole('group', { name: 'Discard your changes' })).toBeTruthy()
  fireEvent.click(within(doc).getByRole('button', { name: 'Keep editing' }))
  fireEvent.click(screen.getByRole('button', { name: /notes\/other\.md/ }))
  expect(within(doc).getByRole('group', { name: 'Discard your changes' })).toBeTruthy()
  expect(screen.getByRole('article', { name: 'notes/plan.md' })).toBeTruthy()
  expect(text().value).toBe('Mine.')

  // Saying yes does what was asked.
  fireEvent.click(within(doc).getByRole('button', { name: 'Discard my changes' }))
  await screen.findByRole('article', { name: 'notes/other.md' })
  expect(screen.queryByRole('article', { name: 'notes/plan.md' })).toBeNull()
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
  expect(screen.queryByRole('group', { name: 'Discard your changes' })).toBeNull()
  expect(within(doc).queryByLabelText('Text')).toBeNull()

  fireEvent.click(within(doc).getByRole('button', { name: 'Edit' }))
  fireEvent.click(within(doc).getByRole('button', { name: 'Back to the documents' }))
  expect(screen.queryByRole('group', { name: 'Discard your changes' })).toBeNull()
  expect(screen.queryByRole('article')).toBeNull()
})

test('Cancel stops an upload, and nothing opens when it would have finished', async () => {
  const held = gate<{ status: number; body: unknown }>()
  const calls = kb({
    [`PUT ${S}/docs/logo.png`]: () => held.answer,
    [`GET ${S}/docs/logo.png`]: { body: { path: 'logo.png', version: V2, size: 3, content_base64: 'AQID' } },
  })
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Upload' }))
  const form = screen.getByRole('form', { name: 'Upload a file' })
  fireEvent.change(within(form).getByLabelText('File'), {
    target: { files: [new File([new Uint8Array([1, 2, 3])], 'logo.png')] },
  })
  fireEvent.click(within(form).getByRole('button', { name: 'Upload' }))
  await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true))
  const put = calls.find((c) => c.method === 'PUT')

  fireEvent.click(within(form).getByRole('button', { name: 'Cancel' }))
  expect(put?.signal?.aborted).toBe(true)
  // The server may well have finished: even then, the file is not opened.
  held.release({ status: 201, body: { commit: 'c1', doc: { path: 'logo.png', version: V2, size: 3 } } })
  await flush()
  expect(screen.queryByRole('article')).toBeNull()
})

test('an older history page that arrives after old versions were removed is dropped', async () => {
  const held = gate<{ body: unknown }>()
  let removed = false
  const entry = (id: string) => commit(id, 'update', { version: V1 })
  kb({
    [`GET ${S}/history/notes/plan.md`]: () =>
      removed ? { body: { commits: [entry('n3')] } } : { body: { commits: [entry('c3')], next: 'c3' } },
    [`GET ${S}/history/notes/plan.md?before=c3`]: () => held.answer,
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

  // The page asked for before the removal names a commit the rewrite made up.
  held.release({ body: { commits: [entry('c2-old')] } })
  await flush()
  expect(within(doc).queryByText('c2-old')).toBeNull()
  expect(within(doc).getByText('n3')).toBeTruthy()
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

test('when older history fails, Reload loads the history again, not the document', async () => {
  let failing = true
  const calls = kb({
    [`GET ${S}/history/notes/plan.md`]: { body: { commits: [commit('c3', 'update', { version: V1 })], next: 'c3' } },
    [`GET ${S}/history/notes/plan.md?before=c3`]: () =>
      failing ? { status: 409, body: { error: 'conflict' } } : { body: { commits: [] } },
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  fireEvent.click(await within(doc).findByRole('button', { name: 'Older' }))
  const alert = await within(doc).findByRole('alert')
  const reads = (path: string) => calls.filter((c) => c.method === 'GET' && c.path === path).length
  expect(reads(`${S}/history/notes/plan.md`)).toBe(1)

  failing = false
  fireEvent.click(within(alert).getByRole('button', { name: 'Reload' }))
  await waitFor(() => expect(reads(`${S}/history/notes/plan.md`)).toBe(2))
  expect(reads(`${S}/docs/notes/plan.md`)).toBe(1)
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
  const reads = (path: string) => calls.filter((c) => c.method === 'GET' && c.path === path).length
  fireEvent.click(within(alert).getByRole('button', { name: 'Reload' }))
  await waitFor(() => expect(reads(`${S}/history/notes/plan.md`)).toBe(2))
  await waitFor(() => expect(reads(`${S}/docs/notes/plan.md`)).toBe(2))
})

test('restoring a deleted document finds the version however far back it is', async () => {
  const kept = (id: string) => commit(id, 'update', { version: V1 })
  const removed = (id: string) => commit(id, 'update', { version: V2, versioned: false })
  const calls = kb({
    [`DELETE ${S}/docs/notes/plan.md`]: { body: { commit: 'c30' } },
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
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'Delete' }))
  fireEvent.click(
    within(screen.getByRole('group', { name: 'Delete the document' })).getByRole('button', { name: 'Delete' }),
  )
  fireEvent.click(await screen.findByRole('button', { name: 'Restore' }))
  await screen.findByRole('article', { name: 'notes/plan.md' })
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ commit: 'c1' })
})

test('restoring a deleted document says so when no version of it is left', async () => {
  kb({
    [`DELETE ${S}/docs/notes/plan.md`]: { body: { commit: 'c30' } },
    [`GET ${S}/history/notes/plan.md`]: { body: { commits: [commit('c30', 'delete')] } },
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'Delete' }))
  fireEvent.click(
    within(screen.getByRole('group', { name: 'Delete the document' })).getByRole('button', { name: 'Delete' }),
  )
  fireEvent.click(await screen.findByRole('button', { name: 'Restore' }))
  await screen.findByText('No earlier version of notes/plan.md was found.')
})

test('a restore that finishes after the notice was left does not open its document', async () => {
  const held = gate<{ body: unknown }>()
  const calls = kb({
    [`DELETE ${S}/docs/notes/plan.md`]: { body: { commit: 'c30' } },
    [`GET ${S}/history/notes/plan.md`]: {
      body: { commits: [commit('c30', 'delete'), commit('c2', 'update', { version: V1 })] },
    },
    [`POST ${S}/restore/notes/plan.md`]: () => held.answer,
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'Delete' }))
  fireEvent.click(
    within(screen.getByRole('group', { name: 'Delete the document' })).getByRole('button', { name: 'Delete' }),
  )
  fireEvent.click(await screen.findByRole('button', { name: 'Restore' }))
  await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))

  // The person opens another document: the notice goes, and the restore ends after that.
  fireEvent.click(screen.getByRole('button', { name: /notes\/other\.md/ }))
  await screen.findByRole('article', { name: 'notes/other.md' })
  held.release({ body: { commit: 'c31', doc: plan } })
  await flush()
  expect(screen.getByRole('article', { name: 'notes/other.md' })).toBeTruthy()
  expect(screen.queryByRole('article', { name: 'notes/plan.md' })).toBeNull()
})

test('a delete that finishes while the document is being edited asks before closing a changed draft', async () => {
  const held = gate<{ body: unknown }>()
  kb({ [`DELETE ${S}/docs/notes/plan.md`]: () => held.answer })
  render(<KnowledgePage workspace={1} />)
  const doc = await open()
  fireEvent.click(await within(doc).findByRole('button', { name: 'Delete' }))
  fireEvent.click(
    within(screen.getByRole('group', { name: 'Delete the document' })).getByRole('button', { name: 'Delete' }),
  )
  // Cancel doesn't stop the delete; the person goes on to edit the document.
  fireEvent.click(within(doc).getByRole('button', { name: 'Cancel' }))
  fireEvent.click(within(doc).getByRole('button', { name: 'Edit' }))
  fireEvent.change(await within(doc).findByLabelText('Text'), { target: { value: 'Mine.' } })
  held.release({ body: { commit: 'c3' } })

  await within(doc).findByRole('group', { name: 'Discard your changes' })
  expect((within(doc).getByLabelText('Text') as HTMLTextAreaElement).value).toBe('Mine.')
})
