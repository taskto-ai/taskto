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

function kb(access: 'write' | 'read', extra: Parameters<typeof fakeFetch>[0] = {}) {
  return fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces: [space(access)] } },
    [`GET ${S}/docs`]: { body: { docs: [plan] } },
    [`GET ${S}/docs/notes/plan.md`]: { body: { ...plan, content: '# Plan\n\nThe **first** draft.\n' } },
    ...extra,
  })
}

async function openPlan() {
  fireEvent.click(await screen.findByRole('button', { name: /^notes/ }))
  fireEvent.click(screen.getByRole('button', { name: /^plan\.md/ }))
  return screen.findByRole('article', { name: 'notes/plan.md' })
}

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

  fireEvent.click(within(doc).getByRole('button', { name: 'Load their version' }))
  expect(text.value).toBe('# Plan\n\nTheirs.\n')
  fireEvent.change(text, { target: { value: 'Mine again.' } })
  current = { ...plan, version: 'd'.repeat(40), content: 'Third.' }
  fireEvent.click(within(doc).getByRole('button', { name: 'Save' }))
  await within(doc).findByRole('button', { name: 'Overwrite with mine' })

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
  expect(document.activeElement).toBe(text)
  expect(screen.queryByRole('dialog')).toBeNull()
  await waitFor(() => expect(document.querySelectorAll('.ui-button-primary')).toHaveLength(1))
  expect(within(doc).getByRole('button', { name: 'Save' }).classList).toContain('ui-button-primary')
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
  const restore = screen.getByRole('button', { name: 'Restore' })
  expect(document.activeElement).toBe(restore)

  fireEvent.click(restore)
  const back = await screen.findByRole('article', { name: 'notes/plan.md' })
  await waitFor(() => expect(document.activeElement).toBe(within(back).getByRole('heading', { level: 2 })))
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ commit: 'c2' })
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
  expect(document.activeElement).toBe(within(doc).getByRole('heading', { level: 2 }))
  expect(calls.filter((c) => c.method === 'POST').map((c) => c.body)).toEqual([
    { commit: 'c1', base_version: V1 },
    { commit: 'c1', base_version: V1 },
  ])
})

const V3 = 'c'.repeat(40)
const entry = (commit: string, day: number, change: 'create' | 'update', version: string, extra = {}) => ({
  commit,
  author: { name: 'Ann', email: 'a@x' },
  time: `2026-01-0${day}T00:00:00Z`,
  message: `Edit ${commit}`,
  change,
  version,
  ...extra,
})
const atCalls = (calls: { path: string }[]) => calls.map((c) => c.path).filter((p) => p.includes('?at='))
const versions = (extra: Parameters<typeof fakeFetch>[0] = {}) => ({
  [`GET ${S}/docs/notes/plan.md`]: { body: { ...plan, content: 'one\nthree\nfour\n' } },
  [`GET ${S}/docs/notes/plan.md?at=c3`]: { body: { ...plan, content: 'one\nthree\nfour\n' } },
  [`GET ${S}/docs/notes/plan.md?at=c2`]: { body: { ...plan, version: V2, content: 'one\ntwo\nthree\n' } },
  [`GET ${S}/docs/notes/plan.md?at=c1`]: { body: { ...plan, version: V3, content: 'one\ntwo\n' } },
  [`GET ${S}/history/notes/plan.md`]: {
    body: { commits: [entry('c3', 3, 'update', V1), entry('c2', 2, 'update', V2), entry('c1', 1, 'create', V3)] },
  },
  ...extra,
})

async function openHistory() {
  const doc = await openPlan()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  await within(doc).findByText('Edit c1')
  return doc
}

test("a change's diff reads that version and the one before it, and shows what was added and removed", async () => {
  const calls = kb('read', versions())
  render(<KnowledgePage workspace={1} />)
  const doc = await openHistory()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[1])
  const diff = await within(doc).findByRole('region', { name: /^Changes on .* by Ann$/ })
  expect((await within(diff).findByText('three')).tagName).toBe('INS')
  expect(within(diff).getByText('1 added')).toBeTruthy()
  expect(within(diff).getByText('0 removed')).toBeTruthy()
  expect(within(diff).getByText('Added:')).toBeTruthy()
  expect(atCalls(calls).sort()).toEqual([`${S}/docs/notes/plan.md?at=c1`, `${S}/docs/notes/plan.md?at=c2`])
  expect(document.activeElement?.textContent).toMatch(/^Changes on/)

  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[0])
  await waitFor(() => expect(within(doc).getByText('four').tagName).toBe('INS'))
  expect(within(doc).getByText('two').tagName).toBe('DEL')
  expect(within(doc).getByText('Removed:')).toBeTruthy()
  expect(within(doc).queryByRole('region', { name: 'As of 2 Jan 2026' })).toBeNull()
  expect(within(doc).getAllByRole('region', { name: /^Changes on/ })).toHaveLength(1)
})

test('a changed line highlights the words that differ', async () => {
  kb(
    'read',
    versions({
      [`GET ${S}/docs/notes/plan.md?at=c3`]: { body: { ...plan, content: 'The quick brown fox\n' } },
      [`GET ${S}/docs/notes/plan.md?at=c2`]: { body: { ...plan, version: V2, content: 'The slow brown fox\n' } },
    }),
  )
  render(<KnowledgePage workspace={1} />)
  const doc = await openHistory()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[0])
  const diff = await within(doc).findByRole('region', { name: /^Changes on/ })
  expect((await within(diff).findByText('slow')).tagName).toBe('DEL')
  expect(within(diff).getByText('quick').tagName).toBe('INS')
  expect(within(diff).getAllByText(/The/)).toHaveLength(2)
})

test('comparing an old version with the current one reads only the old one besides the current', async () => {
  const calls = kb('write', versions())
  render(<KnowledgePage workspace={1} />)
  const doc = await openHistory()
  expect(within(doc).getAllByRole('button', { name: 'Compare with current' })).toHaveLength(2)
  const before = calls.length
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Compare with current' })[1])
  const diff = await within(doc).findByRole('region', { name: /compared with the current version$/ })
  await within(diff).findByText('four')
  expect(within(diff).getByText('two').tagName).toBe('DEL')
  expect(within(diff).getByText('four').tagName).toBe('INS')
  const made = calls.slice(before).map((c) => c.path)
  expect(made).toContain(`${S}/docs/notes/plan.md?at=c1`)
  expect(made).toContain(`${S}/docs/notes/plan.md`)
  expect(made.filter((p) => p.includes('?at=') && !p.endsWith('?at=c1'))).toEqual([])
})

test("the first version's changes are measured against an empty document", async () => {
  const calls = kb('read', versions())
  render(<KnowledgePage workspace={1} />)
  const doc = await openHistory()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[2])
  const diff = await within(doc).findByRole('region', { name: /^Changes on/ })
  expect(await within(diff).findByText('2 added')).toBeTruthy()
  expect(within(diff).getByText('0 removed')).toBeTruthy()
  expect(atCalls(calls)).toEqual([`${S}/docs/notes/plan.md?at=c1`])
})

test('a previous version on the next page of the history is loaded before the diff', async () => {
  const calls = kb(
    'read',
    versions({
      [`GET ${S}/history/notes/plan.md`]: {
        body: { commits: [entry('c3', 3, 'update', V1), entry('c2', 2, 'update', V2)], next: 'c2' },
      },
      [`GET ${S}/history/notes/plan.md?before=c2`]: { body: { commits: [entry('c1', 1, 'create', V3)] } },
    }),
  )
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  await within(doc).findByText('Edit c2')
  expect(calls.some((c) => c.path.includes('before='))).toBe(false)
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[1])
  const diff = await within(doc).findByRole('region', { name: /^Changes on/ })
  expect(await within(diff).findByText('three')).toBeTruthy()
  expect(calls.filter((c) => c.path.includes('before=')).map((c) => c.path)).toEqual([
    `${S}/history/notes/plan.md?before=c2`,
  ])
  expect(atCalls(calls).sort()).toEqual([`${S}/docs/notes/plan.md?at=c1`, `${S}/docs/notes/plan.md?at=c2`])
  await within(doc).findByText('Edit c1')
})

test('a version whose predecessor was removed from history says so instead of diffing against nothing', async () => {
  const calls = kb(
    'read',
    versions({
      [`GET ${S}/history/notes/plan.md`]: {
        body: {
          commits: [entry('c3', 3, 'update', V1), entry('c2', 2, 'update', V2, { versioned: false })],
        },
      },
    }),
  )
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  await within(doc).findByText('Edit c2')
  expect(within(doc).getAllByRole('button', { name: 'Changes' })).toHaveLength(1)
  fireEvent.click(within(doc).getByRole('button', { name: 'Changes' }))
  expect(await within(doc).findByText(/before this one was removed from history/)).toBeTruthy()
  expect(within(doc).queryByRole('region', { name: /^Changes on/ })).toBeNull()
  expect(atCalls(calls)).toEqual([])
})

test('restoring from a diff still asks, then posts the commit and the version it started from', async () => {
  const calls = kb('write', versions({ [`POST ${S}/restore/notes/plan.md`]: { body: { commit: 'c4', doc: plan } } }))
  render(<KnowledgePage workspace={1} />)
  const doc = await openHistory()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Compare with current' })[1])
  const diff = await within(doc).findByRole('region', { name: /compared with the current version$/ })
  fireEvent.click(await within(diff).findByRole('button', { name: 'Restore this version' }))
  const ask = screen.getByRole('alertdialog', { name: 'Restore this version?' })
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)
  fireEvent.click(within(ask).getByRole('button', { name: 'Restore this version' }))
  await waitFor(() => expect(calls.filter((c) => c.method === 'POST')).toHaveLength(1))
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ commit: 'c1', base_version: V1 })
})

test('a diff of the current version offers no restore, and a reader gets none anywhere', async () => {
  kb('read', versions())
  render(<KnowledgePage workspace={1} />)
  const doc = await openHistory()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[0])
  const diff = await within(doc).findByRole('region', { name: /^Changes on/ })
  await within(diff).findByText('four')
  expect(within(diff).queryByRole('button', { name: 'Restore this version' })).toBeNull()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Compare with current' })[0])
  const compared = await within(doc).findByRole('region', { name: /compared with the current version$/ })
  await within(compared).findByText('four')
  expect(within(compared).queryByRole('button', { name: 'Restore this version' })).toBeNull()
})

test('a file that is not Markdown has no diff buttons, though it can still be viewed', async () => {
  kb('read', {
    [`GET ${S}/docs`]: { body: { docs: [{ path: 'data.csv', version: V1, size: 7, versioned: true }] } },
    [`GET ${S}/docs/data.csv`]: {
      body: { path: 'data.csv', version: V1, size: 7, versioned: true, content_base64: 'YSxi' },
    },
    [`GET ${S}/history/data.csv`]: {
      body: { commits: [entry('c2', 2, 'update', V1), entry('c1', 1, 'create', V2)] },
    },
  })
  render(<KnowledgePage workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: /^data\.csv/ }))
  const doc = await screen.findByRole('article', { name: 'data.csv' })
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  await within(doc).findByText('Edit c1')
  expect(within(doc).getAllByRole('button', { name: 'View' })).toHaveLength(2)
  expect(within(doc).queryByRole('button', { name: 'Changes' })).toBeNull()
  expect(within(doc).queryByRole('button', { name: 'Compare with current' })).toBeNull()
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

  await upload(new File([new TextEncoder().encode('héllo')], 'readme.md'))
  await screen.findByRole('article', { name: 'readme.md' })
  expect(calls.filter((c) => c.method === 'PUT').at(-1)?.body).toEqual({ content: 'héllo' })

  exists = true
  const form = await upload(new File([new Uint8Array([1, 2, 3])], 'logo.png'))
  expect(await within(form).findByText('logo.png already exists.')).toBeTruthy()
  fireEvent.click(within(form).getByRole('button', { name: 'Replace it' }))
  await screen.findByRole('article', { name: 'logo.png' })
  expect(lastPut()?.path).toBe(`${S}/docs/logo.png?base_version=${V2}`)
  expect(lastPut()?.body).toBeInstanceOf(Blob)

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

  fireEvent.click(within(doc).getByRole('button', { name: 'History' }))
  const history = await within(doc).findByRole('region', { name: 'History' })
  await within(history).findByText(/Older versions can't be viewed or restored/)
  await within(history).findAllByText('Ann', { exact: false })
  expect(within(history).queryByRole('button', { name: 'View' })).toBeNull()
  expect(within(history).queryByRole('button', { name: 'Restore this version' })).toBeNull()

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

const author = { name: 'Ann', email: 'a@x' }
const lines = (n: number, change?: { at: number; to: string }) =>
  `${Array.from({ length: n }, (_, i) => (change && i + 1 === change.at ? change.to : `line ${i + 1}`)).join('\n')}\n`

test('while a version loads the diff shows no restore and no error, then the lines; a version that fails shows its error', async () => {
  let release: () => void = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  kb(
    'write',
    versions({
      [`GET ${S}/docs/notes/plan.md?at=c1`]: async () => {
        await held
        return { body: { ...plan, version: V3, content: 'one\ntwo\n' } }
      },
    }),
  )
  render(<KnowledgePage workspace={1} />)
  const doc = await openHistory()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Compare with current' })[1])
  const diff = await within(doc).findByRole('region', { name: /compared with the current version$/ })
  expect(within(diff).queryByRole('button', { name: 'Restore this version' })).toBeNull()
  expect(within(diff).queryByRole('alert')).toBeNull()
  expect(within(diff).queryByText('There are no differences.')).toBeNull()
  release()
  expect(await within(diff).findByRole('button', { name: 'Restore this version' })).toBeTruthy()
  expect((await within(diff).findByText('two')).tagName).toBe('DEL')
})

test('a version that cannot be read shows why, and one with no text says it cannot be shown', async () => {
  kb(
    'write',
    versions({
      [`GET ${S}/docs/notes/plan.md?at=c1`]: { status: 500, body: { error: 'the server broke', code: 'x' } },
    }),
  )
  render(<KnowledgePage workspace={1} />)
  const doc = await openHistory()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Compare with current' })[1])
  const diff = await within(doc).findByRole('region', { name: /compared with the current version$/ })
  expect(await within(diff).findByRole('alert')).toBeTruthy()
  expect(within(diff).queryByRole('button', { name: 'Restore this version' })).toBeNull()
  cleanup()

  kb('write', versions({ [`GET ${S}/docs/notes/plan.md?at=c1`]: { body: { ...plan, content_base64: 'YSxi' } } }))
  render(<KnowledgePage workspace={1} />)
  const again = await openHistory()
  fireEvent.click(within(again).getAllByRole('button', { name: 'Compare with current' })[1])
  const second = await within(again).findByRole('region', { name: /compared with the current version$/ })
  expect(await within(second).findByText("This document couldn't be loaded.")).toBeTruthy()
  expect(within(second).queryByRole('button', { name: 'Restore this version' })).toBeNull()
})

test('a change too large to compare says so, and two equal versions say there are no differences', async () => {
  kb(
    'read',
    versions({
      [`GET ${S}/docs/notes/plan.md?at=c2`]: { body: { ...plan, version: V2, content: 'x\n'.repeat(20001) } },
    }),
  )
  render(<KnowledgePage workspace={1} />)
  const doc = await openHistory()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[0])
  const diff = await within(doc).findByRole('region', { name: /^Changes on/ })
  expect(await within(diff).findByText('This change is too large to show.')).toBeTruthy()
  cleanup()

  kb(
    'read',
    versions({
      [`GET ${S}/docs/notes/plan.md?at=c2`]: { body: { ...plan, version: V2, content: 'one\nthree\nfour\n' } },
    }),
  )
  render(<KnowledgePage workspace={1} />)
  const same = await openHistory()
  fireEvent.click(within(same).getAllByRole('button', { name: 'Changes' })[0])
  const equal = await within(same).findByRole('region', { name: /^Changes on/ })
  expect(await within(equal).findByText('There are no differences.')).toBeTruthy()
})

test('unchanged lines far from a change are folded until the person asks for them, and the line numbers are not read out', async () => {
  kb(
    'read',
    versions({
      [`GET ${S}/docs/notes/plan.md?at=c3`]: { body: { ...plan, content: lines(20, { at: 10, to: 'edited' }) } },
      [`GET ${S}/docs/notes/plan.md?at=c2`]: { body: { ...plan, version: V2, content: lines(20) } },
    }),
  )
  render(<KnowledgePage workspace={1} />)
  const doc = await openHistory()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[0])
  const diff = await within(doc).findByRole('region', { name: /^Changes on/ })
  expect(await within(diff).findByText('edited')).toBeTruthy()
  expect(within(diff).queryByText('line 2')).toBeNull()
  expect(within(diff).queryByText('line 19')).toBeNull()
  expect(within(diff).getByText('line 7')).toBeTruthy()
  for (const cell of Array.from(diff.querySelectorAll('.kb-diff-num')))
    expect(cell.getAttribute('aria-hidden')).toBe('true')
  fireEvent.click(within(diff).getByRole('button', { name: 'Show 6 unchanged lines' }))
  expect(within(diff).getByText('line 2')).toBeTruthy()
  expect(within(diff).queryByRole('button', { name: 'Show 6 unchanged lines' })).toBeNull()
  expect(within(diff).queryByText('line 19')).toBeNull()
  fireEvent.click(within(diff).getByRole('button', { name: 'Show 7 unchanged lines' }))
  expect(within(diff).getByText('line 19')).toBeTruthy()
})

test('a text that ends without a newline says so', async () => {
  kb(
    'read',
    versions({
      [`GET ${S}/docs/notes/plan.md?at=c3`]: { body: { ...plan, content: 'one\nthree' } },
      [`GET ${S}/docs/notes/plan.md?at=c2`]: { body: { ...plan, version: V2, content: 'one\nthree\n' } },
    }),
  )
  render(<KnowledgePage workspace={1} />)
  const doc = await openHistory()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[0])
  const diff = await within(doc).findByRole('region', { name: /^Changes on/ })
  expect(await within(diff).findByText('No newline at end of file')).toBeTruthy()
})

test('a version made again after the document was deleted is measured against an empty document, not the life before', async () => {
  const calls = kb('read', {
    [`GET ${S}/docs/notes/plan.md?at=c3`]: { body: { ...plan, content: 'again\n' } },
    [`GET ${S}/docs/notes/plan.md?at=c1`]: { body: { ...plan, version: V3, content: 'before\n' } },
    [`GET ${S}/history/notes/plan.md`]: {
      body: {
        commits: [
          entry('c4', 4, 'update', V1),
          entry('c3', 3, 'create', V2),
          { commit: 'c2', author, time: '2026-01-02T00:00:00Z', message: 'Edit c2', change: 'delete' },
          entry('c1', 1, 'create', V3),
        ],
      },
    },
  })
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  await within(doc).findByText('Edit c1')
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[1])
  const diff = await within(doc).findByRole('region', { name: /^Changes on/ })
  expect(await within(diff).findByText('1 added')).toBeTruthy()
  expect(within(diff).getByText('0 removed')).toBeTruthy()
  expect(atCalls(calls)).toEqual([`${S}/docs/notes/plan.md?at=c3`])
})

test('a page of history that fails to load opens no diff, and says why', async () => {
  const calls = kb(
    'read',
    versions({
      [`GET ${S}/history/notes/plan.md`]: {
        body: { commits: [entry('c3', 3, 'update', V1), entry('c2', 2, 'update', V2)], next: 'c2' },
      },
      [`GET ${S}/history/notes/plan.md?before=c2`]: { status: 500, body: { error: 'no history', code: 'x' } },
    }),
  )
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  await within(doc).findByText('Edit c2')
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[1])
  expect(await within(doc).findByRole('alert')).toBeTruthy()
  expect(within(doc).queryByRole('region', { name: /^Changes on/ })).toBeNull()
  expect(atCalls(calls)).toEqual([])
})

test('Changes, Compare with current and View each open once and close when pressed again, and one replaces another', async () => {
  kb('read', versions())
  render(<KnowledgePage workspace={1} />)
  const doc = await openHistory()
  const pressed = (name: string, index: number) =>
    within(doc).getAllByRole('button', { name })[index].getAttribute('aria-pressed')
  const regions = () => within(doc).queryAllByRole('region', { name: /^Changes on|compared with the current|^As of/ })

  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[1])
  await within(doc).findByRole('region', { name: /^Changes on/ })
  expect(pressed('Changes', 1)).toBe('true')
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[1])
  expect(regions()).toHaveLength(0)
  expect(pressed('Changes', 1)).toBe('false')

  fireEvent.click(within(doc).getAllByRole('button', { name: 'Compare with current' })[1])
  await within(doc).findByRole('region', { name: /compared with the current version$/ })
  expect(pressed('Compare with current', 1)).toBe('true')
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Compare with current' })[1])
  expect(regions()).toHaveLength(0)

  fireEvent.click(within(doc).getAllByRole('button', { name: 'View' })[1])
  await within(doc).findByRole('region', { name: /^As of/ })
  expect(pressed('View', 1)).toBe('true')
  fireEvent.click(within(doc).getAllByRole('button', { name: 'View' })[1])
  expect(regions()).toHaveLength(0)

  fireEvent.click(within(doc).getAllByRole('button', { name: 'View' })[1])
  await within(doc).findByRole('region', { name: /^As of/ })
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[1])
  await within(doc).findByRole('region', { name: /^Changes on/ })
  expect(regions()).toHaveLength(1)
})

test('a diff of the current version offers no restore to someone who can write, an older one does, and switching the comparison moves the focus to the new heading', async () => {
  kb('write', versions())
  render(<KnowledgePage workspace={1} />)
  const doc = await openHistory()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[0])
  const current = await within(doc).findByRole('region', { name: /^Changes on/ })
  await within(current).findByText('four')
  expect(within(current).queryByRole('button', { name: 'Restore this version' })).toBeNull()

  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[1])
  const currentName = current.getAttribute('aria-label')
  await waitFor(() =>
    expect(
      within(doc)
        .getByRole('region', { name: /^Changes on/ })
        .getAttribute('aria-label'),
    ).not.toBe(currentName),
  )
  const older = within(doc).getByRole('region', { name: /^Changes on/ })
  expect(await within(older).findByRole('button', { name: 'Restore this version' })).toBeTruthy()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Compare with current' })[1])
  await within(doc).findByRole('region', { name: /compared with the current version$/ })
  await waitFor(() => expect(document.activeElement?.textContent).toMatch(/compared with the current version$/))
})

test('taking old versions out of history closes a diff that was open', async () => {
  let removed = false
  kb('write', {
    ...versions(),
    [`DELETE ${S}/history/notes/plan.md`]: () => {
      removed = true
      return { body: { commit: 'c4', doc: plan, removed: 2 } }
    },
  })
  render(<KnowledgePage workspace={1} admin />)
  const doc = await openHistory()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[1])
  await within(doc).findByRole('region', { name: /^Changes on/ })
  fireEvent.click(within(doc).getByRole('button', { name: 'Remove old versions from history…' }))
  const panel = within(doc).getByRole('alertdialog', { name: 'Remove the old versions from history' })
  fireEvent.click(within(panel).getByRole('button', { name: 'Remove old versions' }))
  expect(await within(doc).findByText('Removed 2 old versions from the history.')).toBeTruthy()
  expect(removed).toBe(true)
  expect(within(doc).queryByRole('region', { name: /^Changes on/ })).toBeNull()
})

test('Changes waits on the page of older history it needs, shows it is busy, and asking for the same page meanwhile makes no second request', async () => {
  let release: () => void = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  const calls = kb(
    'read',
    versions({
      [`GET ${S}/history/notes/plan.md`]: {
        body: { commits: [entry('c3', 3, 'update', V1), entry('c2', 2, 'update', V2)], next: 'c2' },
      },
      [`GET ${S}/history/notes/plan.md?before=c2`]: async () => {
        await held
        return { body: { commits: [entry('c1', 1, 'create', V3)] } }
      },
    }),
  )
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  await within(doc).findByText('Edit c2')
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[1])
  await waitFor(() =>
    expect(
      within(doc)
        .getAllByRole('button', { name: /^Changes/ })[1]
        .getAttribute('aria-busy'),
    ).toBe('true'),
  )
  fireEvent.click(within(doc).getByRole('button', { name: /^Older/ }))
  release()
  await within(doc).findByRole('region', { name: /^Changes on/ })
  expect(calls.filter((c) => c.path.includes('before='))).toHaveLength(1)
  expect(within(doc).getAllByRole('button', { name: 'Changes' })[1].getAttribute('aria-pressed')).toBe('true')
})

test('choosing Compare with current or View while Changes waits on older history keeps that choice when the page arrives', async () => {
  for (const choice of ['Compare with current', 'View']) {
    let release: () => void = () => {}
    const held = new Promise<void>((resolve) => {
      release = resolve
    })
    kb(
      'read',
      versions({
        [`GET ${S}/history/notes/plan.md`]: {
          body: { commits: [entry('c3', 3, 'update', V1), entry('c2', 2, 'update', V2)], next: 'c2' },
        },
        [`GET ${S}/history/notes/plan.md?before=c2`]: async () => {
          await held
          return { body: { commits: [entry('c1', 1, 'create', V3)] } }
        },
      }),
    )
    render(<KnowledgePage workspace={1} />)
    const doc = await openPlan()
    fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
    await within(doc).findByText('Edit c2')
    fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[1])
    await waitFor(() =>
      expect(
        within(doc)
          .getAllByRole('button', { name: /^Changes/ })[1]
          .getAttribute('aria-busy'),
      ).toBe('true'),
    )
    fireEvent.click(within(doc).getAllByRole('button', { name: choice })[choice === 'View' ? 1 : 0])
    const chosen = choice === 'View' ? /^As of/ : /compared with the current version$/
    await within(doc).findByRole('region', { name: chosen })
    release()
    await within(doc).findByText('Edit c1')
    await act(async () => {})
    expect(within(doc).getByRole('region', { name: chosen })).toBeTruthy()
    expect(within(doc).queryByRole('region', { name: /^Changes on/ })).toBeNull()
    expect(
      within(doc)
        .getAllByRole('button', { name: /^Changes/ })[1]
        .getAttribute('aria-busy'),
    ).not.toBe('true')
    cleanup()
  }
})

test('choosing View clears the note about a removed earlier version', async () => {
  kb(
    'read',
    versions({
      [`GET ${S}/history/notes/plan.md`]: {
        body: { commits: [entry('c3', 3, 'update', V1), entry('c2', 2, 'update', V2, { versioned: false })] },
      },
    }),
  )
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  await within(doc).findByText('Edit c2')
  fireEvent.click(within(doc).getByRole('button', { name: 'Changes' }))
  expect(await within(doc).findByText(/before this one was removed from history/)).toBeTruthy()
  fireEvent.click(within(doc).getByRole('button', { name: 'View' }))
  expect(within(doc).queryByText(/before this one was removed from history/)).toBeNull()
})

test('switching one version from its changes to its comparison with the current one moves the focus to the new heading', async () => {
  kb('read', versions())
  render(<KnowledgePage workspace={1} />)
  const doc = await openHistory()
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[1])
  await within(doc).findByRole('region', { name: /^Changes on/ })
  const compare = within(doc).getAllByRole('button', { name: 'Compare with current' })[0]
  compare.focus()
  expect(document.activeElement).toBe(compare)
  fireEvent.click(compare)
  await within(doc).findByRole('region', { name: /compared with the current version$/ })
  await waitFor(() => expect(document.activeElement?.tagName).toBe('H3'))
  expect(document.activeElement?.textContent).toMatch(/compared with the current version$/)
})

test('older history that is already being loaded is not asked for again by Changes, and arrives once', async () => {
  let release: () => void = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  const calls = kb(
    'read',
    versions({
      [`GET ${S}/history/notes/plan.md`]: {
        body: { commits: [entry('c3', 3, 'update', V1), entry('c2', 2, 'update', V2)], next: 'c2' },
      },
      [`GET ${S}/history/notes/plan.md?before=c2`]: async () => {
        await held
        return { body: { commits: [entry('c1', 1, 'create', V3)] } }
      },
    }),
  )
  render(<KnowledgePage workspace={1} />)
  const doc = await openPlan()
  fireEvent.click(await within(doc).findByRole('button', { name: 'History' }))
  await within(doc).findByText('Edit c2')
  fireEvent.click(within(doc).getByRole('button', { name: /^Older/ }))
  fireEvent.click(within(doc).getAllByRole('button', { name: 'Changes' })[1])
  release()
  await within(doc).findByText('Edit c1')
  expect(calls.filter((c) => c.path.includes('before='))).toHaveLength(1)
  expect(within(doc).getAllByText('Edit c1')).toHaveLength(1)
})
