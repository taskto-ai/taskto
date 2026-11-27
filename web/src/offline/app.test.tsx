// @vitest-environment jsdom

import 'fake-indexeddb/auto'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { App } from '../App'
import { ApiError, api, forAccount, SESSION_LOST, sessionLostFor } from '../api'
import { setPreferences } from '../i18n'
import { type BeforeLeave, useBeforeLeave } from '../router'
import { sameSession, sessionUser } from '../session'
import { fakeFetch } from '../test/fakes'
import { toast } from '../ui/Toast'
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
  expect(calls.some((c) => c.path === '/api/logout')).toBe(false)
  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(screen.queryByRole('alertdialog')).toBeNull()
  expect(await runtime.store.listItems()).toHaveLength(1)
  logOut()
  await screen.findByRole('alertdialog')
  fireEvent.click(screen.getByRole('button', { name: 'Discard and sign out' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  await screen.findByRole('heading', { name: /sign in|log in/i })
  expect((await indexedDB.databases()).map((d) => d.name)).not.toContain('taskto-offline-7')
})

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
  act(() => {
    window.dispatchEvent(sessionLostFor(99))
  })
  await new Promise((resolve) => setTimeout(resolve, 20))
  expect(screen.queryByRole('heading', { name: /sign in|log in/i })).toBeNull()
  expect(screen.getByRole('banner')).toBeTruthy()
  expect(runtimeState().runtime?.engine.status().paused).not.toBe('session')
  act(() => {
    window.dispatchEvent(sessionLostFor(7))
  })
  await screen.findByRole('heading', { name: /sign in|log in/i })
})

test('a request made for another account that is refused as signed out, answered after this one signed in, leaves it signed in', async () => {
  fakeFetch({
    ...signedIn,
    'POST /api/me/deletion': { status: 401, body: { error: 'log in first', code: 'not_logged_in' } },
  })
  await signedInAs()
  await expect(api('/api/me/deletion', { method: 'POST', body: {}, headers: forAccount(99) })).rejects.toBeInstanceOf(
    ApiError,
  )
  await new Promise((resolve) => setTimeout(resolve, 20))
  expect(screen.queryByRole('heading', { name: /sign in|log in/i })).toBeNull()
  expect(screen.getByRole('banner')).toBeTruthy()
  expect(runtimeState().runtime?.engine.status().paused).not.toBe('session')
  await expect(api('/api/me/deletion', { method: 'POST', body: {}, headers: forAccount(7) })).rejects.toBeInstanceOf(
    ApiError,
  )
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
  await screen.findByRole('heading', { name: /sign in|log in/i })
  expect(runtime.engine.status().paused).toBe('session')
  expect((await runtime.store.listItems())[0]).toMatchObject({ state: 'queued', attempts: 0 })
  const posts = calls.filter((c) => c.method === 'POST' && c.path === `${base}/actions`).length
  expect(posts).toBe(1)
  session = true
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'pat@example.com' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-test-password' } })
  fireEvent.click(screen.getByRole('button', { name: /sign in|log in/i }))
  await waitFor(async () => expect((await runtime.store.listItems())[0].state).toBe('synced'))
  expect(calls.filter((c) => c.method === 'POST' && c.path === `${base}/actions`)).toHaveLength(2)
})

test('with no server, the apps downloaded here open, and a record made there is kept for later', async () => {
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

const LAST_KEY = 'taskto:offline-last'

test('a tab that learns another account signed in shows only that, and nothing of the account’s records', async () => {
  fakeFetch({ ...signedIn })
  await signedInAs()
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  runtime.engine.pause('manual')
  await runtime.store.enqueue(queued(), [])
  window.localStorage.setItem(LAST_KEY, JSON.stringify({ id: 8, name: 'Bob' }))
  fireEvent(window, new StorageEvent('storage', { key: LAST_KEY, newValue: localStorage.getItem(LAST_KEY) }))
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toMatch(/The account changed/)
  expect(screen.queryByRole('banner')).toBeNull()
  expect(document.body.textContent).not.toContain('Lunch')
  expect(runtimeState().runtime).toBeNull()
  await waitFor(() => expect(runtime.store.listItems()).rejects.toThrow())
  expect(screen.getByRole('button', { name: 'Reload this page' })).toBeTruthy()
})

test('another account signed in from another tab ends this page’s session: its work and toasts stop', async () => {
  fakeFetch({ ...signedIn })
  await signedInAs()
  const still = sameSession()
  toast('Something of the account before')
  expect(await screen.findByText('Something of the account before')).toBeTruthy()
  window.localStorage.setItem(LAST_KEY, JSON.stringify({ id: 8, name: 'Bob' }))
  fireEvent(window, new StorageEvent('storage', { key: LAST_KEY, newValue: localStorage.getItem(LAST_KEY) }))
  await screen.findByRole('alertdialog')
  expect(still()).toBe(false)
  expect(screen.queryByText('Something of the account before')).toBeNull()
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
  const reopened = await OfflineStore.open(7)
  expect((await reopened.listItems()).map((i) => i.state)).toEqual(['queued'])
  reopened.close()
})

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
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toMatch(/may not have reached the server/)
  expect(dialog.textContent).toMatch(/newer version/)
  expect(calls.some((c) => c.path === '/api/logout')).toBe(false)
  fireEvent.click(screen.getByRole('button', { name: 'Keep them and sign out' }))
  await screen.findByRole('heading', { name: /sign in|log in/i })
  expect(calls.some((c) => c.path === '/api/logout')).toBe(true)
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

test('another account signing in while this tab checks for unsent records before logging out ends its session for good', async () => {
  fakeFetch({ ...signedIn, 'POST /api/logout': { status: 204 } })
  await signedInAs()
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  runtime.engine.pause('manual')
  await runtime.store.enqueue(queued(), [])
  logOut()
  window.localStorage.setItem(LAST_KEY, JSON.stringify({ id: 8, name: 'Bob' }))
  fireEvent(window, new StorageEvent('storage', { key: LAST_KEY, newValue: localStorage.getItem(LAST_KEY) }))
  expect((await screen.findByRole('alertdialog')).textContent).toMatch(/The account changed/)
  await new Promise((r) => setTimeout(r, 50))
  expect(sessionUser()).toBeNull()
})

test('Log out, asked about unsent records, Cancel: nothing ended, and a language pick on its way finishes as it would', async () => {
  let saved: (() => void) | null = null
  const calls = fakeFetch({
    ...signedIn,
    'POST /api/logout': { status: 204 },
    'PATCH /api/me': () =>
      new Promise((done) => {
        saved = () => done({ body: { ...me, user: { ...me.user, locale: 'pt-BR' } } })
      }),
  })
  await signedInAs()
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  runtime.engine.pause('manual')
  await runtime.store.enqueue(queued(), [])
  fireEvent.click(screen.getByRole('button', { name: /^Account: / }))
  fireEvent.click(screen.getByRole('menuitemradio', { name: 'Português (Brasil)' }))
  await vi.waitFor(() => expect(saved).not.toBeNull())
  const started = sameSession()
  fireEvent.click(screen.getByRole('button', { name: /^(Account|Conta): / }))
  fireEvent.click(screen.getByRole('menuitem', { name: 'Sair' }))
  await screen.findByRole('alertdialog')
  fireEvent.click(screen.getByRole('button', { name: 'Cancelar' }))
  expect(started()).toBe(true)
  expect(sessionUser()).toBe(7)
  await act(async () => saved?.())
  fireEvent.click(screen.getByRole('button', { name: /^Conta: / }))
  const checked = screen
    .getAllByRole('menuitemradio')
    .filter((r) => r.getAttribute('aria-checked') === 'true')
    .map((r) => r.textContent)
  expect(checked).toContain('Português (Brasil)')
  expect(calls.some((c) => c.path === '/api/logout')).toBe(false)
  await act(() => setPreferences({ user: null }))
})

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
  expect(ask).toHaveBeenCalledTimes(1)
  await act(async () => {})
  expect(screen.queryByRole('alertdialog')).toBeNull()
  act(() => leave?.())
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog.textContent).toMatch(/1 record on this device hasn’t reached the server/)
  expect(ask).toHaveBeenCalledTimes(1)
  expect(calls.some((c) => c.path === '/api/logout')).toBe(false)
})

test('one sign-out asks about a draft and then the records, each once; Cancel at either leaves the session and the draft as they were', async () => {
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
  const started = sameSession()
  const records = () => screen.queryAllByRole('alertdialog').length

  logOut()
  await act(async () => {})
  expect(ask).toHaveBeenCalledTimes(1)
  expect(records()).toBe(0)
  expect(started()).toBe(true)

  logOut()
  expect(ask).toHaveBeenCalledTimes(2)
  act(() => leave?.())
  await screen.findByRole('alertdialog')
  await act(async () => {})
  expect(records()).toBe(1)
  expect(ask).toHaveBeenCalledTimes(2)

  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(records()).toBe(0)
  expect(started()).toBe(true)
  expect(sessionUser()).toBe(7)
  logOut()
  expect(ask).toHaveBeenCalledTimes(3)
  expect(calls.some((c) => c.path === '/api/logout')).toBe(false)
})

test('"Discard and sign out" with a logout the server refuses discards nothing: the records are still there', async () => {
  fakeFetch({
    ...signedIn,
    'POST /api/logout': { status: 500, body: { error: 'internal error', code: 'internal' } },
    [`POST ${base}/actions`]: { status: 503, body: { error: 'busy' } },
  })
  await signedInAs()
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  runtime.engine.pause('manual')
  await runtime.store.enqueue(queued(), [])
  logOut()
  await screen.findByRole('alertdialog')
  fireEvent.click(screen.getByRole('button', { name: 'Discard and sign out' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  expect(await runtime.store.listItems()).toHaveLength(1)
  expect(lastUser()).toEqual({ id: 7, name: 'Pat' })
})

test('while signing out waits for the server, the queue starts no send, even when the connection comes back', async () => {
  let confirm: (() => void) | null = null
  const calls = fakeFetch({
    ...signedIn,
    'POST /api/logout': () =>
      new Promise((done) => {
        confirm = () => done({ status: 204 })
      }),
    [`POST ${base}/actions`]: { body: [] },
  })
  await signedInAs()
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  Object.defineProperty(navigator, 'onLine', { configurable: true, value: false })
  await runtime.store.enqueue(queued(), [])
  const sends = () => calls.filter((c) => c.method === 'POST' && c.path === `${base}/actions`).length
  logOut()
  await screen.findByRole('alertdialog')
  fireEvent.click(screen.getByRole('button', { name: 'Discard and sign out' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  await waitFor(() => expect(confirm).not.toBeNull())
  Object.defineProperty(navigator, 'onLine', { configurable: true, value: true })
  act(() => {
    window.dispatchEvent(new Event('online'))
  })
  await new Promise((r) => setTimeout(r, 50))
  expect(sends()).toBe(0)
  await act(async () => confirm?.())
  await screen.findByRole('heading', { name: /sign in|log in/i })
  Reflect.deleteProperty(navigator, 'onLine')
})

test('a sign-out cut short by the tab closing leaves nothing an offline start would open: the account stays closed until it is finished', async () => {
  let confirm: (() => void) | null = null
  fakeFetch({
    ...signedIn,
    'POST /api/logout': () =>
      new Promise((done) => {
        confirm = () => done({ status: 204 })
      }),
  })
  await signedInAs()
  logOut()
  await waitFor(() => expect(confirm).not.toBeNull())
  cleanup()
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))),
  )
  render(<App />)
  expect(await screen.findByRole('heading', { name: "Can't reach the server" })).toBeTruthy()
  expect(screen.queryByRole('heading', { name: 'Offline' })).toBeNull()
})
