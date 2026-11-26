// @vitest-environment jsdom
//
// The app's own use of the offline machinery: signing in claims the account's database, signing
// out asks about records not sent and deletes what the device kept, and with no server the
// downloaded apps open.

import 'fake-indexeddb/auto'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { App } from '../App'
import { SESSION_LOST, sessionLostFor } from '../api'
import { type BeforeLeave, useBeforeLeave } from '../router'
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
  'GET /api/workspaces/1/updates': { body: { configured: false, running_version: 'dev', status: null } },
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

const queued = (): Omit<OutboxItem, 'seq' | 'version'> => ({
  kind: 'insert',
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

// Log out, from the account menu at the sidebar's foot.
function logOut() {
  fireEvent.click(screen.getByRole('button', { name: /^Account: / }))
  fireEvent.click(screen.getByRole('menuitem', { name: 'Log out' }))
}

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
  logOut()
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
  logOut()
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toMatch(/1 record on this device hasn’t reached the server/)
  // Nothing was signed out yet.
  expect(calls.some((c) => c.path === '/api/logout')).toBe(false)
  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(screen.queryByRole('alertdialog')).toBeNull()
  expect(await runtime.store.listItems()).toHaveLength(1)
  // Discard and sign out: the record and the database go.
  logOut()
  await screen.findByRole('alertdialog')
  fireEvent.click(screen.getByRole('button', { name: 'Discard and sign out' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  await screen.findByRole('heading', { name: /sign in|log in/i })
  expect((await indexedDB.databases()).map((d) => d.name)).not.toContain('taskto-offline-7')
})

// The person deletes their account in the settings, with an unsent record, a draft and an offline
// database on this device. `refusedFirst`: another request of the app, made while the server was
// ending the session, is answered 401 before the answer to the deletion arrives, so the app is on
// the sign-in screen, no longer listening for the session to end, when it does.
async function deleteAccountHere(refusedFirst: boolean) {
  sessionStorage.clear()
  window.history.replaceState(null, '', '/w/1/settings/data')
  fakeFetch({
    ...signedIn,
    'GET /api/me/deletion': {
      body: { grace_days: 14, method: 'password', two_factor: false, fresh: false, blockers: [] },
    },
    'POST /api/me/deletion': () => {
      if (refusedFirst) window.dispatchEvent(new Event(SESSION_LOST))
      return { status: 202, body: { purge_at: '2026-12-08T10:00:00.000Z' } }
    },
  })
  render(<App />)
  await waitFor(() => expect(runtimeState().runtime?.user).toBe(7))
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  runtime.engine.pause('manual')
  await runtime.store.enqueue(queued(), [])
  window.localStorage.setItem('taskto:message:7:1', 'half a message')
  expect((await indexedDB.databases()).map((d) => d.name)).toContain('taskto-offline-7')

  fireEvent.click(await screen.findByRole('button', { name: 'Delete my account…' }))
  const form = await screen.findByRole('form', { name: 'Delete your account' })
  fireEvent.change(within(form).getByLabelText('Your password'), { target: { value: 'fake-test-password' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Delete my account' }))

  await screen.findByRole('heading', { name: /sign in|log in/i })
  await waitFor(async () => expect((await indexedDB.databases()).map((d) => d.name)).not.toContain('taskto-offline-7'))
  expect(window.localStorage.getItem('taskto:message:7:1')).toBeNull()
  expect(lastUser()).toBeNull()
  sessionStorage.clear()
}

test('deleting the account here clears what the device kept of it: the offline database with its unsent records, and the drafts', async () => {
  await deleteAccountHere(false)
})

test('the end of another account’s session, heard after this one signed in, leaves it signed in', async () => {
  fakeFetch({ ...signedIn, 'POST /api/logout': { status: 204 } })
  await signedInAs()
  // A deleted account's answer arriving late says its own account's session ended: not this one's.
  act(() => {
    window.dispatchEvent(sessionLostFor(99))
  })
  await new Promise((resolve) => setTimeout(resolve, 20))
  expect(screen.queryByRole('heading', { name: /sign in|log in/i })).toBeNull()
  expect(screen.getByRole('banner')).toBeTruthy()
  expect(runtimeState().runtime?.engine.status().paused).not.toBe('session')
  // The same event for this account (7) takes it to the sign-in screen.
  act(() => {
    window.dispatchEvent(sessionLostFor(7))
  })
  await screen.findByRole('heading', { name: /sign in|log in/i })
})

test('the same when a request refused a moment earlier has already taken the app to the sign-in screen', async () => {
  await deleteAccountHere(true)
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

// Another account's unsent records are on this browser: nothing of them is shown, nothing else is
// either, until the person signing in chooses.
async function sealedUnsentOf(user: number) {
  const store = await OfflineStore.open(user)
  await store.putDownload({ ...download, user } as never)
  await store.enqueue({ ...queued(), user }, [])
  store.close()
  window.localStorage.setItem('taskto:offline-users', JSON.stringify([user]))
}

test('another account’s unsent records block the person signing in until they discard them', async () => {
  await sealedUnsentOf(1)
  fakeFetch({ ...signedIn, 'POST /api/logout': { status: 204 } })
  render(<App />)
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toMatch(/1 unsent record of another account/)
  // Nothing of the app is drawn behind it, and nothing of the other account is readable.
  expect(screen.queryByRole('banner')).toBeNull()
  expect(document.body.textContent).not.toContain('Lunch')
  expect((await runtimeStateItems()).length).toBe(0)
  fireEvent.click(screen.getByRole('button', { name: 'Discard them' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  await screen.findByRole('banner')
  expect((await indexedDB.databases()).map((d) => d.name)).not.toContain('taskto-offline-1')
})

test('…or sign out, to sign in as that account and send them', async () => {
  await sealedUnsentOf(1)
  fakeFetch({ ...signedIn, 'POST /api/logout': { status: 204 } })
  render(<App />)
  await screen.findByRole('alertdialog')
  fireEvent.click(screen.getByRole('button', { name: 'Sign out to sign in as that account' }))
  await screen.findByRole('heading', { name: /sign in|log in/i })
  // Hers are kept for her; this account's own database is gone.
  const names = (await indexedDB.databases()).map((d) => d.name)
  expect(names).toContain('taskto-offline-1')
  expect(names).not.toContain('taskto-offline-7')
})

async function runtimeStateItems() {
  return (await runtimeState().runtime?.store.listItems()) ?? []
}

test('signing out from an invite page asks about unsent records and deletes the database', async () => {
  window.history.replaceState(null, '', '/invite#token-token')
  const store = await OfflineStore.open(7)
  await store.enqueue(queued(), [])
  store.close()
  window.localStorage.setItem('taskto:offline-users', '[7]')
  const calls = fakeFetch({ ...signedIn, 'POST /api/logout': { status: 204 } })
  render(<App />)
  fireEvent.click(await screen.findByRole('button', { name: /^Log out, to use another account/ }))
  // The same warning as anywhere else, and nothing was signed out yet.
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toMatch(/1 record on this device hasn’t reached the server/)
  expect(calls.some((c) => c.path === '/api/logout')).toBe(false)
  fireEvent.click(screen.getByRole('button', { name: 'Discard and sign out' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  await waitFor(async () => expect((await indexedDB.databases()).map((d) => d.name)).not.toContain('taskto-offline-7'))
  expect(calls.some((c) => c.path === '/api/logout')).toBe(true)
  expect(lastUser()).toBeNull()
})

test('signing out from an invite page with nothing unsent leaves nothing of the account on the device', async () => {
  window.history.replaceState(null, '', '/invite#token-token')
  const store = await OfflineStore.open(7)
  await store.putDownload(download as never)
  store.close()
  window.localStorage.setItem('taskto:offline-users', '[7]')
  rememberUser({ id: 7, name: 'Pat' })
  fakeFetch({ ...signedIn, 'POST /api/logout': { status: 204 } })
  render(<App />)
  fireEvent.click(await screen.findByRole('button', { name: /^Log out, to use another account/ }))
  await waitFor(async () => expect((await indexedDB.databases()).map((d) => d.name)).not.toContain('taskto-offline-7'))
  expect(lastUser()).toBeNull()
})

// --- Third round of review.

const LAST_KEY = 'taskto:offline-last'

test('a tab that learns another account signed in shows only that, and nothing of the account’s records', async () => {
  fakeFetch({ ...signedIn })
  await signedInAs()
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  runtime.engine.pause('manual')
  await runtime.store.enqueue(queued(), [])
  // Another tab signs in as someone else.
  window.localStorage.setItem(LAST_KEY, JSON.stringify({ id: 8, name: 'Bob' }))
  fireEvent(window, new StorageEvent('storage', { key: LAST_KEY }))
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toMatch(/The account changed/)
  expect(screen.queryByRole('banner')).toBeNull()
  expect(document.body.textContent).not.toContain('Lunch')
  expect(runtimeState().runtime).toBeNull()
  // Its connection to the database is closed: nothing more can be read through it.
  await waitFor(() => expect(runtime.store.listItems()).rejects.toThrow())
  expect(screen.getByRole('button', { name: 'Reload this page' })).toBeTruthy()
})

test('a send refused because the session is another account’s shows the same, and keeps the record', async () => {
  fakeFetch({
    ...signedIn,
    [`POST ${base}/actions`]: { status: 409, body: { error: 'another account', code: 'session_account_changed' } },
  })
  await signedInAs()
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  runtime.engine.pause('manual')
  await runtime.store.enqueue(queued(), [])
  runtime.engine.resume()
  await screen.findByRole('alertdialog')
  expect(screen.queryByRole('banner')).toBeNull()
  // The record is still on the device, for its owner.
  const reopened = await OfflineStore.open(7)
  expect((await reopened.listItems()).map((i) => i.state)).toEqual(['queued'])
  reopened.close()
})

// A database written by a newer version than this page.
async function newerDatabase(user: number) {
  await new Promise<void>((resolve, reject) => {
    const open = indexedDB.open(`taskto-offline-${user}`, 99)
    open.onupgradeneeded = () => open.result.createObjectStore('future')
    open.onsuccess = () => {
      open.result.close()
      resolve()
    }
    open.onerror = () => reject(open.error)
  })
  window.localStorage.setItem('taskto:offline-users', JSON.stringify([user]))
}

test('signing out when the outbox can’t be read warns that unsent records may exist, and keeps the database if asked', async () => {
  await newerDatabase(7)
  const calls = fakeFetch({ ...signedIn, 'POST /api/logout': { status: 204 } })
  render(<App />)
  await screen.findByRole('banner')
  logOut()
  // Not zero: nobody can say. The person is told, and nothing was signed out yet.
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toMatch(/may not have reached the server/)
  expect(dialog.textContent).toMatch(/newer version/)
  expect(calls.some((c) => c.path === '/api/logout')).toBe(false)
  fireEvent.click(screen.getByRole('button', { name: 'Keep them and sign out' }))
  await screen.findByRole('heading', { name: /sign in|log in/i })
  expect(calls.some((c) => c.path === '/api/logout')).toBe(true)
  // Kept, and listed, for the next version to read: nothing was deleted.
  expect((await indexedDB.databases()).map((d) => d.name)).toContain('taskto-offline-7')
  expect(JSON.parse(window.localStorage.getItem('taskto:offline-users') ?? '[]')).toContain(7)
  expect(lastUser()).toBeNull()
})

test('…and discards it only when the person says so, after being asked', async () => {
  await newerDatabase(7)
  fakeFetch({ ...signedIn, 'POST /api/logout': { status: 204 } })
  render(<App />)
  await screen.findByRole('banner')
  logOut()
  await screen.findByRole('alertdialog')
  // Cancelling keeps everything.
  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(screen.queryByRole('alertdialog')).toBeNull()
  expect((await indexedDB.databases()).map((d) => d.name)).toContain('taskto-offline-7')
  logOut()
  await screen.findByRole('alertdialog')
  fireEvent.click(screen.getByRole('button', { name: 'Discard and sign out' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  await screen.findByRole('heading', { name: /sign in|log in/i })
  expect((await indexedDB.databases()).map((d) => d.name)).not.toContain('taskto-offline-7')
})

// --- Fourth round of review.

test('signing out names the account it is for, and a stale tab’s sign-out leaves the session that is there now alone', async () => {
  const calls = fakeFetch({ ...signedIn, 'POST /api/logout': { status: 204 } })
  await signedInAs()
  logOut()
  await screen.findByRole('heading', { name: /sign in|log in/i })
  const out = calls.find((c) => c.path === '/api/logout')
  expect(out?.headers['x-taskto-expected-user']).toBe('7')
})

test('…and when the server says the session is another account’s, nobody is signed out here', async () => {
  const calls = fakeFetch({
    ...signedIn,
    'POST /api/logout': { status: 409, body: { error: 'another account', code: 'session_account_changed' } },
  })
  await signedInAs()
  logOut()
  // This tab learns it is stale: nothing of the account is shown, and the sign-out went no further.
  await screen.findByRole('alertdialog')
  expect(calls.some((c) => c.path === '/api/logout')).toBe(true)
  expect(screen.queryByRole('heading', { name: /sign in|log in/i })).toBeNull()
})

test('a record that appears after the person was asked is never deleted by the sign-out: they are asked again', async () => {
  const calls = fakeFetch({ ...signedIn, 'POST /api/logout': { status: 204 } })
  await signedInAs()
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  runtime.engine.pause('manual')
  // Nothing unsent when the person is asked; another tab of the account commits a record right after.
  const stale = await OfflineStore.open(7)
  const unsynced = runtime.store.unsynced.bind(runtime.store)
  runtime.store.unsynced = async () => {
    const seen = await unsynced()
    await stale.enqueue(queued(), [])
    return seen
  }
  logOut()
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toMatch(/1 record on this device hasn’t reached the server/)
  expect(calls.some((c) => c.path === '/api/logout')).toBe(false)
  stale.close()
  expect((await indexedDB.databases()).map((d) => d.name)).toContain('taskto-offline-7')
  expect(await runtime.store.listItems()).toHaveLength(1)
})

// A page holding a draft: its question when the person would leave.
function Draft({ ask }: { ask: BeforeLeave }) {
  useBeforeLeave(ask)
  return null
}

test('signing out over a draft and records not sent asks about the draft first, then the records, each once', async () => {
  const calls = fakeFetch({
    ...signedIn,
    'POST /api/logout': { status: 204 },
    [`POST ${base}/actions`]: { status: 503, body: { error: 'busy' } },
  })
  let leave: (() => void) | null = null
  const ask = vi.fn<BeforeLeave>((go) => {
    leave = go
  })
  render(
    <>
      <App />
      <Draft ask={ask} />
    </>,
  )
  await waitFor(() => expect(runtimeState().runtime?.user).toBe(7))
  await screen.findByRole('banner')
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  runtime.engine.pause('manual')
  await runtime.store.enqueue(queued(), [])
  logOut()
  // The draft's question, alone: the device's comes only once the person means to go.
  expect(ask).toHaveBeenCalledTimes(1)
  await act(async () => {})
  expect(screen.queryByRole('alertdialog')).toBeNull()
  act(() => leave?.())
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toMatch(/1 record on this device hasn’t reached the server/)
  expect(ask).toHaveBeenCalledTimes(1)
  expect(calls.some((c) => c.path === '/api/logout')).toBe(false)
})
