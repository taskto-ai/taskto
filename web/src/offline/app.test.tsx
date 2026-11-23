// @vitest-environment jsdom
//
// The app's own use of the offline machinery: signing in claims the account's database, signing
// out asks about records not sent and deletes what the device kept, and with no server the
// downloaded apps open.

import 'fake-indexeddb/auto'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { App } from '../App'
import { fakeFetch } from '../test/fakes'
import { deactivate, lastUser, rememberUser, runtimeState } from './runtime'
import { OfflineStore, type OutboxItem } from './store'

const me = {
  user: { id: 7, email: 'pat@example.com', name: 'Pat' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
}
const base = '/api/workspaces/1/spaces/2/apps/1'

const download = {
  workspace: 1,
  space: 2,
  app: 1,
  key: '1/2/1',
  user: 7,
  name: 'Expenses',
  release: 3,
  definition: {
    definition_version: 1,
    requires: [],
    collection: {
      fields: [
        { name: 'description', type: 'string', required: true },
        { name: 'amount', type: 'number', required: true },
      ],
    },
    actions: { add_expense: { kind: 'insert' }, delete_expense: { kind: 'delete' } },
    summaries: { total: { kind: 'sum', field: 'amount' } },
  },
  messages: surface,
  scopeOf: 'all',
  records: 1,
  total: 1,
  truncated: false,
  canInsert: true,
  capturedAt: '2026-11-02T10:00:00.000Z',
  bytes: 10,
} as const

const signedIn = {
  'GET /api/setup': { body: { needed: false } },
  'GET /api/me': { body: me },
  'GET /api/workspaces/1/conversations': { body: { conversations: [] } },
  'GET /api/workspaces/1/backup': { body: { health: 'none' } },
}

beforeEach(() => {
  globalThis.indexedDB = new IDBFactory()
  window.history.replaceState(null, '', '/')
  window.localStorage.clear()
})

afterEach(async () => {
  cleanup()
  vi.unstubAllGlobals()
  await deactivate({ purge: false })
})

const queued = (): Omit<OutboxItem, 'seq'> => ({
  id: 'item-1',
  user: 7,
  scopeKey: '1/2/1',
  workspace: 1,
  space: 2,
  app: 1,
  base,
  release: 3,
  schema: 1,
  action: {
    name: 'add_expense',
    surfaceId: 'app-1',
    sourceComponentId: 'add',
    timestamp: '2026-11-02T10:00:00.000Z',
    context: { description: 'Lunch', amount: '5' },
  },
  operation: 'op-app-test-0001',
  record: { id: -5, revision: null },
  attachments: [],
  state: 'queued',
  attempts: 0,
  createdAt: '2026-11-02T10:00:00.000Z',
  updatedAt: '2026-11-02T10:00:00.000Z',
})

async function signedInAs() {
  render(<App />)
  await waitFor(() => expect(runtimeState().runtime?.user).toBe(7))
  await screen.findByRole('banner')
}

test('signing in claims the account’s offline database, and signing out with nothing unsent deletes it', async () => {
  const calls = fakeFetch({ ...signedIn, 'POST /api/logout': { status: 204 } })
  await signedInAs()
  expect(lastUser()).toEqual({ id: 7, name: 'Pat' })
  expect((await indexedDB.databases()).map((d) => d.name)).toContain('taskto-offline-7')
  fireEvent.click(screen.getByRole('button', { name: 'Log out' }))
  await screen.findByRole('heading', { name: /sign in|log in/i })
  expect(calls.some((c) => c.path === '/api/logout')).toBe(true)
  expect((await indexedDB.databases()).map((d) => d.name)).not.toContain('taskto-offline-7')
  expect(lastUser()).toBeNull()
})

test('signing out with records not sent asks first: cancel keeps everything, discard deletes it', async () => {
  const calls = fakeFetch({
    ...signedIn,
    'POST /api/logout': { status: 204 },
    [`POST ${base}/actions`]: { status: 503, body: { error: 'busy' } },
  })
  await signedInAs()
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  runtime.engine.pause('manual')
  await runtime.store.enqueue(queued(), [])
  fireEvent.click(screen.getByRole('button', { name: 'Log out' }))
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toMatch(/1 record on this device hasn’t reached the server/)
  // Nothing was signed out yet.
  expect(calls.some((c) => c.path === '/api/logout')).toBe(false)
  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(screen.queryByRole('alertdialog')).toBeNull()
  expect(await runtime.store.listItems()).toHaveLength(1)
  // Discard and sign out: the record and the database go.
  fireEvent.click(screen.getByRole('button', { name: 'Log out' }))
  await screen.findByRole('alertdialog')
  fireEvent.click(screen.getByRole('button', { name: 'Discard and sign out' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  await screen.findByRole('heading', { name: /sign in|log in/i })
  expect((await indexedDB.databases()).map((d) => d.name)).not.toContain('taskto-offline-7')
})

test('an ended session leaves the queue where it is, and signing in again sends it', async () => {
  let session = true
  const calls = fakeFetch({
    ...signedIn,
    'GET /api/me': () => (session ? { body: me } : { status: 401, body: { error: 'sign in' } }),
    [`POST ${base}/actions`]: () => (session ? { body: [] } : { status: 401, body: { error: 'sign in' } }),
    'POST /api/login': () => {
      session = true
      return { body: me }
    },
  })
  await signedInAs()
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  session = false
  runtime.engine.pause('manual')
  await runtime.store.enqueue(queued(), [])
  runtime.engine.resume()
  // The server says the session is over: the app goes to the sign-in, sending is paused, the record waits.
  await screen.findByRole('heading', { name: /sign in|log in/i })
  expect(runtime.engine.status().paused).toBe('session')
  expect((await runtime.store.listItems())[0]).toMatchObject({ state: 'queued', attempts: 0 })
  const posts = calls.filter((c) => c.method === 'POST' && c.path === `${base}/actions`).length
  expect(posts).toBe(1)
  // Signing in again (the same person) sends it, once.
  session = true
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'pat@example.com' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-test-password' } })
  fireEvent.click(screen.getByRole('button', { name: /sign in|log in/i }))
  await waitFor(async () => expect((await runtime.store.listItems())[0].state).toBe('synced'))
  expect(calls.filter((c) => c.method === 'POST' && c.path === `${base}/actions`)).toHaveLength(2)
})

test('with no server, the apps downloaded here open, and a record made there is kept for later', async () => {
  // Pat signed in here before, and downloaded an app.
  rememberUser({ id: 7, name: 'Pat' })
  const store = await OfflineStore.open(7)
  await store.putDownload(download as never)
  store.close()
  window.localStorage.setItem('taskto:offline-users', '[7]')
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))),
  )
  render(<App />)
  await screen.findByRole('heading', { name: 'Offline' })
  fireEvent.click(await screen.findByRole('button', { name: 'Open Expenses' }))
  await screen.findByText('Expense tracker')
  expect(screen.getByRole('note').textContent).toMatch(/not the server’s totals/)
  const input = (label: string) => {
    const el = screen.getByText(label).closest('a2ui-basic-textfield')?.querySelector('input')
    if (!el) throw new Error(label)
    return el
  }
  fireEvent.input(input('Description'), { target: { value: 'Bus' } })
  fireEvent.input(input('Amount'), { target: { value: '2.5' } })
  fireEvent.click(screen.getByRole('button', { name: 'Add' }))
  expect((await screen.findByText(/Saved on this device/)).textContent).toBeTruthy()
  await waitFor(async () => expect(await runtimeState().runtime?.store.listItems()).toHaveLength(1))
  // It is listed with its state, and it is the only thing that was kept.
  expect((await screen.findAllByText('Saved on this device, waiting to sync')).length).toBeGreaterThan(0)
})

test('with no server and nobody known here, the app says it can’t reach it', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))),
  )
  render(<App />)
  expect((await screen.findByRole('alert')).textContent).toMatch(/Can't reach the server/)
})
