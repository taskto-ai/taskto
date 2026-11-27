// @vitest-environment jsdom
//
// A sign-out on a browser two people share, with the offline database there: what one person
// agreed to lose is gone once their login is over, whichever way it ends; what they did not agree
// to lose is kept; and nothing of a newer login (the same person, signed in again) is touched.

import 'fake-indexeddb/auto'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { App } from '../App'
import { api } from '../api'
import { cleanUpAfterSignOut, finishUnfinishedSignOuts } from '../signOut'
import { fakeFetch } from '../test/fakes'
import { pendingSignOuts } from './pendingSignOut'
import { activate, deactivate, lastUser, runtimeState } from './runtime'
import { OfflineStore, type OutboxItem } from './store'

// How long a logout is waited for: long by default, so a logout a test holds is never cut short by
// a loaded machine; a test that wants it to time out says so (`deadline.logout = 0`).
const deadline = vi.hoisted(() => ({ logout: 60_000 }))
vi.mock('../signOut', async (original) => {
  const real = await original<typeof import('../signOut')>()
  return {
    ...real,
    get SIGN_OUT_WAIT_MS() {
      return deadline.logout
    },
  }
})

const me = {
  user: { id: 7, email: 'pat@example.com', name: 'Pat' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
  session: 's1',
}
const base = '/api/workspaces/1/spaces/2/apps/1'

const signedIn = {
  'GET /api/setup': { body: { needed: false } },
  'GET /api/workspaces/1/conversations': { body: { conversations: [] } },
  'GET /api/workspaces/1/backup': { body: { health: 'none' } },
  'GET /api/workspaces/1/updates': { body: { configured: false, running_version: 'dev', status: null } },
}

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
  operation: 'op-device-test-0001',
  record: { id: -5, revision: null },
  attachments: [],
  state: 'queued',
  attempts: 0,
  createdAt: '2026-11-02T10:00:00.000Z',
  updatedAt: '2026-11-02T10:00:00.000Z',
})

const pending = () => pendingSignOuts().map((p) => `${p.user}:${p.session}`)
const databases = async () => (await indexedDB.databases()).map((d) => d.name)

beforeEach(() => {
  deadline.logout = 60_000
  globalThis.indexedDB = new IDBFactory()
  window.history.replaceState(null, '', '/')
  window.localStorage.clear()
  window.sessionStorage.clear()
})

afterEach(async () => {
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  Reflect.deleteProperty(navigator, 'onLine')
  await deactivate({ purge: false })
})

function logOut() {
  fireEvent.click(screen.getByRole('button', { name: /^Account: / }))
  fireEvent.click(screen.getByRole('menuitem', { name: 'Log out' }))
}

// Signed in with one record the server hasn't had, which the person is asked about and agrees to
// lose; the device has no connection, so the queue sends nothing on its own.
async function signOutDiscarding() {
  render(<App />)
  await waitFor(() => expect(runtimeState().runtime?.user).toBe(7))
  await screen.findByRole('banner')
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  Object.defineProperty(navigator, 'onLine', { configurable: true, value: false })
  await runtime.store.enqueue(queued(), [])
  logOut()
  await screen.findByRole('alertdialog')
  fireEvent.click(screen.getByRole('button', { name: 'Discard and sign out' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  return runtime
}

test('a logout that got no answer and reaches the server after Back finishes the sign-out the person chose', async () => {
  // The logout's answer doesn't come in time.
  deadline.logout = 0
  let session: 'on' | 'over' = 'on'
  fakeFetch({
    ...signedIn,
    'GET /api/me': () => (session === 'on' ? { body: me } : { status: 401, body: { error: 'not logged in' } }),
    // The logout is held somewhere on the way: the page never hears from it.
    'POST /api/logout': () => new Promise(() => {}),
  })
  await signOutDiscarding()
  fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
  await screen.findByRole('navigation', { name: 'Sections' })
  // The shell's effects (the listener for a session that ends among them) are in place.
  await act(async () => {})
  // The stalled logout reaches the server now: the login is over, and the next request says so.
  session = 'over'
  await act(async () => {
    await api('/api/me').catch(() => {})
  })
  await screen.findByRole('heading', { name: 'Log in' })
  // The device is cleared as the person decided: the record they agreed to lose is not kept to be
  // sent at the next sign-in, and an offline start doesn't open the account.
  await waitFor(() => expect(pending()).toEqual([]))
  expect(lastUser()).toBeNull()
  expect(await databases()).not.toContain('taskto-offline-7')
})

test('a logout the server refused, then Back, is no sign-out: a session that ends later leaves the device for the next sign-in', async () => {
  let session: 'on' | 'over' = 'on'
  fakeFetch({
    ...signedIn,
    'GET /api/me': () => (session === 'on' ? { body: me } : { status: 401, body: { error: 'not logged in' } }),
    'POST /api/logout': { status: 503, body: { error: 'unavailable', code: 'internal' } },
  })
  const runtime = await signOutDiscarding()
  fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
  await screen.findByRole('navigation', { name: 'Sections' })
  // The shell's effects (the listener for a session that ends among them) are in place.
  await act(async () => {})
  expect(pending()).toEqual([])
  session = 'over'
  await act(async () => {
    await api('/api/me').catch(() => {})
  })
  await screen.findByRole('heading', { name: 'Log in' })
  expect(lastUser()).toEqual({ id: 7, name: 'Pat' })
  const store = await OfflineStore.open(7)
  expect(await store.listItems()).toHaveLength(1)
  store.close()
  void runtime
})

test('another tab finishing the same sign-out takes this one to the login even with the offline database open, and it stays there', async () => {
  let confirm: (() => void) | null = null
  fakeFetch({
    ...signedIn,
    'GET /api/me': { body: me },
    'POST /api/logout': () =>
      new Promise((done) => {
        confirm = () => done({ status: 204 })
      }),
  })
  render(<App />)
  await waitFor(() => expect(runtimeState().runtime?.user).toBe(7))
  await screen.findByRole('banner')
  logOut()
  await waitFor(() => expect(confirm).not.toBeNull())
  // The other tab's cleanup forgets the account: nobody is signed in on this browser now.
  localStorage.removeItem('taskto:offline-last')
  window.dispatchEvent(new StorageEvent('storage', { key: 'taskto:offline-last', newValue: null }))
  await screen.findByRole('heading', { name: 'Log in' })
  expect(screen.queryByText('The account changed')).toBeNull()
  // This tab's own logout never gets an answer: the page stays where it is.
  await new Promise((r) => setTimeout(r, 150))
  expect(screen.getByRole('heading', { name: 'Log in' })).toBeTruthy()
  expect(screen.queryByText('The account changed')).toBeNull()
})

test('a logout confirmed after the same person signed in again in another tab leaves that newer login’s drafts and data', async () => {
  let confirm: (() => void) | null = null
  fakeFetch({
    ...signedIn,
    'GET /api/me': { body: me },
    'POST /api/logout': () =>
      new Promise((done) => {
        confirm = () => done({ status: 204 })
      }),
  })
  render(<App />)
  await waitFor(() => expect(runtimeState().runtime?.user).toBe(7))
  await screen.findByRole('banner')
  logOut()
  await waitFor(() => expect(confirm).not.toBeNull())
  // Meanwhile another tab signs the same person in with a new login, as its start does, and they
  // begin a draft there.
  await finishUnfinishedSignOuts({ ...me, session: 's2' } as never)
  localStorage.setItem('taskto:builder-start:7:1', 'a draft of the newer login')
  await act(async () => confirm?.())
  await screen.findByRole('heading', { name: 'Log in' })
  expect(localStorage.getItem('taskto:builder-start:7:1')).toBe('a draft of the newer login')
  expect(lastUser()).toEqual({ id: 7, name: 'Pat' })
  expect(await databases()).toContain('taskto-offline-7')
  expect(pending()).toEqual([])
})

test('a sign-out whose cleanup cannot read the queue keeps the database: a record saved after the person was asked is not lost', async () => {
  await activate(7, 'Pat')
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  runtime.engine.pause('manual')
  await runtime.store.enqueue(queued(), [])
  const read = vi.spyOn(OfflineStore.prototype, 'retireOrKeepUnsent').mockRejectedValue(new Error('read failed'))
  await deactivate({ purge: true, user: 7, approved: new Map() })
  read.mockRestore()
  expect(await databases()).toContain('taskto-offline-7')
  const store = await OfflineStore.open(7)
  expect(await store.listItems()).toHaveLength(1)
  store.close()
})

test('a database the person was told could not be read, and chose to discard, goes even when it still can’t be read', async () => {
  await activate(7, 'Pat')
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  runtime.engine.pause('manual')
  await runtime.store.enqueue(queued(), [])
  vi.spyOn(OfflineStore.prototype, 'retireOrKeepUnsent').mockRejectedValue(new Error('read failed'))
  await deactivate({ purge: true, user: 7, approved: new Map(), discardUnreadable: true })
  expect(await databases()).not.toContain('taskto-offline-7')
})

test('“Discard and sign out” for a database the person was told can’t be read deletes it after the server confirms', async () => {
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me }, 'POST /api/logout': { status: 204 } })
  render(<App />)
  await waitFor(() => expect(runtimeState().runtime?.user).toBe(7))
  await screen.findByRole('banner')
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  runtime.engine.pause('manual')
  await runtime.store.enqueue(queued(), [])
  // The database can't be read: the person is told so when they sign out, and at the cleanup still.
  vi.spyOn(OfflineStore.prototype, 'unsynced').mockRejectedValue(new Error('read failed'))
  vi.spyOn(OfflineStore.prototype, 'unapproved').mockRejectedValue(new Error('read failed'))
  vi.spyOn(OfflineStore.prototype, 'retireOrKeepUnsent').mockRejectedValue(new Error('read failed'))
  logOut()
  await screen.findByRole('alertdialog')
  fireEvent.click(screen.getByRole('button', { name: 'Discard and sign out' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  await screen.findByRole('heading', { name: 'Log in' })
  await waitFor(async () => expect(await databases()).not.toContain('taskto-offline-7'))
  expect(pending()).toEqual([])
})

test('a newer login of the account that starts while the cleanup waits keeps its drafts and data', async () => {
  await activate(7, 'Pat')
  runtimeState().runtime?.engine.pause('manual')
  await finishUnfinishedSignOuts(me as never)
  localStorage.setItem('taskto:drafts-of', '7')
  const cleaning = cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map() })
  // Before the cleanup's first wait is over, another tab starts with a newer login of the same
  // account, and a draft is begun there.
  await finishUnfinishedSignOuts({ ...me, session: 's2' } as never)
  localStorage.setItem('taskto:builder-start:7:1', 'a draft of the newer login')
  expect(await cleaning).toBe(true)
  expect(localStorage.getItem('taskto:builder-start:7:1')).toBe('a draft of the newer login')
  expect(lastUser()).toEqual({ id: 7, name: 'Pat' })
  expect(await databases()).toContain('taskto-offline-7')
})
