// @vitest-environment jsdom

import 'fake-indexeddb/auto'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { App } from '../App'
import { api, type Me } from '../api'
import { cleanUpAfterSignOut, finishUnfinishedSignOuts } from '../signOut'
import { fakeFetch } from '../test/fakes'
import {
  backFromSignOut,
  browserLogin,
  forgetLogin,
  pendingSignOutOf,
  pendingSignOuts,
  recordSignOut,
  rememberLogin,
  signOutPending,
  unansweredSignOut,
} from './pendingSignOut'
import { activate, deactivate, discardForeign, lastUser, runtimeState } from './runtime'
import { databaseExists, lockTimes, OfflineStore, type OutboxItem } from './store'

const during = vi.hoisted(() => ({
  claim: null as (() => void) | null,
  claimed: null as (() => void) | null,
  activate: null as (() => void) | null,
}))
vi.mock('./runtime', async (original) => {
  const real = await original<typeof import('./runtime')>()
  return {
    ...real,
    activate: async (...args: Parameters<typeof real.activate>) => {
      const hook = during.activate
      during.activate = null
      hook?.()
      return real.activate(...args)
    },
  }
})
const deadline = vi.hoisted(() => ({ logout: 60_000 }))
vi.mock('../signOut', async (original) => {
  const real = await original<typeof import('../signOut')>()
  return {
    ...real,
    get SIGN_OUT_WAIT_MS() {
      return deadline.logout
    },
    finishUnfinishedSignOuts: async (...args: Parameters<typeof real.finishUnfinishedSignOuts>) => {
      const hook = during.claim
      during.claim = null
      hook?.()
      const out = await real.finishUnfinishedSignOuts(...args)
      const after = during.claimed
      during.claimed = null
      after?.()
      return out
    },
  }
})

const hold = vi.hoisted(() => ({ preferences: null as Promise<void> | null, reached: false }))
vi.mock('../i18n', async (original) => {
  const real = await original<typeof import('../i18n')>()
  return {
    ...real,
    setPreferences: async (...args: Parameters<typeof real.setPreferences>) => {
      if (hold.preferences) {
        hold.reached = true
        await hold.preferences
      }
      return real.setPreferences(...args)
    },
  }
})

const me = {
  user: { id: 7, email: 'pat@example.com', name: 'Pat' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
  session: 's1',
} as Me
const base = '/api/workspaces/1/spaces/2/apps/1'
const signedIn = {
  'GET /api/setup': { body: { needed: false } },
  'GET /api/workspaces/1/conversations': { body: { conversations: [] } },
  'GET /api/workspaces/1/backup': { body: { health: 'none' } },
  'GET /api/workspaces/1/updates': { body: { configured: false, running_version: 'dev', status: null } },
}

const queued = (id = 'item-1'): Omit<OutboxItem, 'seq' | 'version'> => ({
  kind: 'insert',
  id,
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
  operation: `op-race-${id}`,
  record: { id: -5, revision: null },
  attachments: [],
  state: 'queued',
  attempts: 0,
  createdAt: '2026-11-02T10:00:00.000Z',
  updatedAt: '2026-11-02T10:00:00.000Z',
})

const databases = async () => (await indexedDB.databases()).map((d) => d.name)
const LAST = 'taskto:offline-last'

function fakeLocks() {
  const names: string[] = []
  const tails = new Map<string, Promise<unknown>>()
  Object.defineProperty(navigator, 'locks', {
    configurable: true,
    value: {
      request: (name: string, a: unknown, b?: unknown) => {
        const work = (typeof a === 'function' ? a : b) as () => Promise<unknown>
        const signal = typeof a === 'function' ? undefined : (a as { signal?: AbortSignal } | undefined)?.signal
        names.push(name)
        let given = false
        const turn = (tails.get(name) ?? Promise.resolve()).then(() => {
          given = true
          if (signal?.aborted) throw new DOMException('aborted', 'AbortError')
          return work()
        })
        tails.set(
          name,
          turn.catch(() => {}),
        )
        if (!signal) return turn
        return Promise.race([
          turn,
          new Promise((_, reject) =>
            signal.addEventListener('abort', () => {
              if (!given) reject(new DOMException('aborted', 'AbortError'))
            }),
          ),
        ])
      },
    },
  })
  return names
}

beforeEach(() => {
  deadline.logout = 60_000
  globalThis.indexedDB = new IDBFactory()
  window.history.replaceState(null, '', '/')
  window.localStorage.clear()
  window.sessionStorage.clear()
  hold.preferences = null
  during.claim = null
  during.claimed = null
  during.activate = null
})

const times = { ...lockTimes }
afterEach(async () => {
  Object.assign(lockTimes, times)
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  Reflect.deleteProperty(navigator, 'locks')
  hold.preferences = null
  await deactivate({ purge: false })
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

async function backWithPreferencesHeld() {
  fakeFetch({
    ...signedIn,
    'GET /api/me': { body: me },
    'POST /api/logout': { status: 503, body: { error: 'unavailable', code: 'internal' } },
  })
  await signedInAs()
  logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  let release: () => void = () => {}
  hold.preferences = new Promise((done) => {
    release = () => {
      hold.preferences = null
      done()
    }
  })
  hold.reached = false
  fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
  await waitFor(() => expect(hold.reached).toBe(true))
  return release
}

test('a newer login that is over shields nothing: Try again, answered 401 for it, clears the device', async () => {
  let logout: { status: number; body: unknown } = { status: 503, body: { error: 'unavailable', code: 'internal' } }
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me }, 'POST /api/logout': () => logout })
  await signedInAs()
  logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  await finishUnfinishedSignOuts({ ...me, session: 's2' })
  logout = { status: 401, body: { error: 'not logged in' } }
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
  await screen.findByRole('heading', { name: 'Log in' })
  await waitFor(() => expect(lastUser()).toBeNull())
  expect(await databases()).not.toContain('taskto-offline-7')
  expect(signOutPending(7)).toBe(false)
})

test('a login whose session is lost is forgotten: a late cleanup of an older login is not shielded by it', async () => {
  fakeFetch({ ...signedIn, 'GET /api/me': { body: { ...me, session: 's2' } } })
  await signedInAs()
  await screen.findByRole('navigation', { name: 'Sections' })
  await act(async () => {})
  fakeFetch({ ...signedIn, 'GET /api/me': { status: 401, body: { error: 'not logged in' } } })
  await act(async () => {
    await api('/api/me').catch(() => {})
  })
  await screen.findByRole('heading', { name: 'Log in' })
  expect(await cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map() })).toBe(true)
  expect(lastUser()).toBeNull()
  expect(await databases()).not.toContain('taskto-offline-7')
})

test("a newer login's database, made again while the cleanup waited, is not deleted with the retired one", async () => {
  await activate(7, 'Pat')
  runtimeState().runtime?.engine.pause('manual')
  const real = OfflineStore.prototype.retireOrKeepUnsent
  vi.spyOn(OfflineStore.prototype, 'retireOrKeepUnsent').mockImplementation(async function (
    this: OfflineStore,
    ...args: Parameters<typeof real>
  ) {
    const out = await real.apply(this, args)
    const fresh = await OfflineStore.open(7)
    await fresh.enqueue(queued('newer'), [])
    fresh.close()
    return out
  })
  await cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map() })
  vi.restoreAllMocks()
  const store = await OfflineStore.open(7)
  expect((await store.listItems()).map((i) => i.id)).toEqual(['newer'])
  store.close()
})

test("opening an account's database and a sign-out's deletion of it take the same lock, one at a time", async () => {
  const names = fakeLocks()
  await activate(7, 'Pat')
  runtimeState().runtime?.engine.pause('manual')
  await cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map() })
  expect(names.filter((n) => n === 'taskto-offline-db:7').length).toBeGreaterThanOrEqual(2)
  expect(await databases()).not.toContain('taskto-offline-7')
})

test('a cleanup that keeps a database it could not read is not verified clear: it is said, and finished later', async () => {
  recordSignOut(7, 's1', false, new Map())
  await activate(7, 'Pat')
  runtimeState().runtime?.engine.pause('manual')
  vi.spyOn(OfflineStore.prototype, 'retireOrKeepUnsent').mockRejectedValue(new Error('read failed'))
  vi.spyOn(OfflineStore.prototype, 'unapproved').mockRejectedValue(new Error('read failed'))
  expect(await cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map() })).toBe(false)
  expect(pendingSignOutOf(7)).toMatchObject({ session: 's1' })
  expect(await databases()).toContain('taskto-offline-7')
})

test('a database that could not be read is not verified clear on a browser that cannot list its databases', async () => {
  recordSignOut(7, 's1', false, new Map())
  await activate(7, 'Pat')
  runtimeState().runtime?.engine.pause('manual')
  vi.spyOn(OfflineStore.prototype, 'retireOrKeepUnsent').mockRejectedValue(new Error('read failed'))
  vi.spyOn(OfflineStore.prototype, 'unapproved').mockRejectedValue(new Error('read failed'))
  Object.defineProperty(indexedDB, 'databases', { configurable: true, value: undefined })
  expect(await cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map() })).toBe(false)
  expect(pendingSignOutOf(7)).toMatchObject({ session: 's1' })
})

test('a database the sign-out could not delete is not verified clear', async () => {
  recordSignOut(7, 's1', false, new Map())
  await activate(7, 'Pat')
  runtimeState().runtime?.engine.pause('manual')
  await deactivate({ purge: false, user: 7 })
  vi.spyOn(indexedDB, 'deleteDatabase').mockImplementation(() => {
    const request = { error: new DOMException('refused', 'UnknownError') } as unknown as IDBOpenDBRequest
    setTimeout(() => request.onerror?.(new Event('error')), 0)
    return request
  })
  expect(await cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map() })).toBe(false)
  expect(pendingSignOutOf(7)).toMatchObject({ session: 's1' })
})

test('one the person chose to discard, that could not be deleted, is not verified clear even if it reads now', async () => {
  recordSignOut(7, 's1', false, new Map(), true)
  await activate(7, 'Pat')
  const runtime = runtimeState().runtime
  runtime?.engine.pause('manual')
  await runtime?.store.enqueue(queued(), [])
  await deactivate({ purge: false, user: 7 })
  vi.spyOn(OfflineStore.prototype, 'retireOrKeepUnsent').mockRejectedValue(new Error('read failed'))
  vi.spyOn(indexedDB, 'deleteDatabase').mockImplementation(() => {
    const request = { error: new DOMException('refused', 'UnknownError') } as unknown as IDBOpenDBRequest
    setTimeout(() => request.onerror?.(new Event('error')), 0)
    return request
  })
  expect(await cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map(), discardUnreadable: true })).toBe(false)
  expect(pendingSignOutOf(7)).toMatchObject({ session: 's1' })
})

test("a cleanup of one login never settles another login's sign-out recorded while it waited", async () => {
  recordSignOut(7, 's1', false, new Map())
  await activate(7, 'Pat')
  runtimeState().runtime?.engine.pause('manual')
  const cleaning = cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map() })
  recordSignOut(7, 's2', false, new Map())
  await cleaning
  expect(pendingSignOutOf(7)).toMatchObject({ session: 's2' })
})

test('Back stops before starting the app again when another tab finished the sign-out meanwhile', async () => {
  const release = await backWithPreferencesHeld()
  localStorage.removeItem(LAST)
  window.dispatchEvent(new StorageEvent('storage', { key: LAST, newValue: null }))
  await screen.findByRole('heading', { name: 'Log in' })
  await act(async () => release())
  await new Promise((r) => setTimeout(r, 50))
  expect(screen.getByRole('heading', { name: 'Log in' })).toBeTruthy()
  expect(screen.queryByRole('navigation', { name: 'Sections' })).toBeNull()
  expect(lastUser()).toBeNull()
})

test('Back stops before starting the app again when another person signed in meanwhile: their drafts and marker stay', async () => {
  const release = await backWithPreferencesHeld()
  localStorage.setItem('taskto:drafts-of', '8')
  localStorage.setItem('taskto:builder-start:8:1', 'a draft of the second person')
  const marker = JSON.stringify({ id: 8, name: 'Sam' })
  localStorage.setItem(LAST, marker)
  window.dispatchEvent(new StorageEvent('storage', { key: LAST, newValue: marker }))
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await act(async () => release())
  await new Promise((r) => setTimeout(r, 50))
  expect(localStorage.getItem('taskto:drafts-of')).toBe('8')
  expect(localStorage.getItem('taskto:builder-start:8:1')).toBe('a draft of the second person')
  expect(lastUser()).toEqual({ id: 8, name: 'Sam' })
})

test('Back that started the app again just as another tab finished the sign-out lets go of what it took', async () => {
  let report: (() => void) | undefined
  let holdReport = false
  const fresh = { ...me, user: { ...me.user, time_zone: '' } } as Me
  fakeFetch({
    ...signedIn,
    'GET /api/me': { body: fresh },
    'PATCH /api/me': () =>
      holdReport
        ? new Promise((done) => {
            report = () => done({ body: { ...fresh, user: { ...fresh.user, time_zone: 'UTC' } } })
          })
        : { body: { ...fresh, user: { ...fresh.user, time_zone: 'UTC' } } },
    'POST /api/logout': { status: 503, body: { error: 'unavailable', code: 'internal' } },
  })
  await signedInAs()
  logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  holdReport = true
  fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
  await waitFor(() => expect(report).toBeDefined())
  localStorage.removeItem(LAST)
  window.dispatchEvent(new StorageEvent('storage', { key: LAST, newValue: null }))
  await screen.findByRole('heading', { name: 'Log in' })
  await act(async () => report?.())
  await waitFor(() => expect(runtimeState().runtime).toBeNull())
  expect(lastUser()).toBeNull()
  expect(screen.getByRole('heading', { name: 'Log in' })).toBeTruthy()
  expect(screen.queryByRole('navigation', { name: 'Sections' })).toBeNull()
})

test('Back in the tab that signed out sends the queue again only once the sign-out is let go', async () => {
  fakeFetch({
    ...signedIn,
    'GET /api/me': { body: me },
    'POST /api/logout': { status: 503, body: { error: 'unavailable', code: 'internal' } },
  })
  await signedInAs()
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  const resume = runtime.engine.resume.bind(runtime.engine)
  const pendingAtResume: boolean[] = []
  vi.spyOn(runtime.engine, 'resume').mockImplementation(() => {
    pendingAtResume.push(signOutPending(7))
    resume()
  })
  logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
  await screen.findByRole('navigation', { name: 'Sections' })
  expect(pendingAtResume.length).toBeGreaterThan(0)
  expect(pendingAtResume).not.toContain(true)
})

test('Back whose claim of the account comes just after another tab finished the sign-out takes the marker back away', async () => {
  fakeFetch({
    ...signedIn,
    'GET /api/me': { body: me },
    'POST /api/logout': { status: 503, body: { error: 'unavailable', code: 'internal' } },
  })
  await signedInAs()
  logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  during.activate = () => {
    localStorage.removeItem(LAST)
    window.dispatchEvent(new StorageEvent('storage', { key: LAST, newValue: null }))
  }
  fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
  await screen.findByRole('heading', { name: 'Log in' })
  await waitFor(() => expect(lastUser()).toBeNull())
  expect(screen.queryByRole('navigation', { name: 'Sections' })).toBeNull()
})

test('one the person chose to discard is not verified clear while it may still be there, even where databases cannot be listed', async () => {
  recordSignOut(7, 's1', false, new Map(), true)
  await activate(7, 'Pat')
  const runtime = runtimeState().runtime
  runtime?.engine.pause('manual')
  await runtime?.store.enqueue(queued(), [])
  await deactivate({ purge: false, user: 7 })
  vi.spyOn(OfflineStore.prototype, 'retireOrKeepUnsent').mockRejectedValue(new Error('read failed'))
  vi.spyOn(indexedDB, 'deleteDatabase').mockImplementation(() => {
    const request = { error: new DOMException('refused', 'UnknownError') } as unknown as IDBOpenDBRequest
    setTimeout(() => request.onerror?.(new Event('error')), 0)
    return request
  })
  Object.defineProperty(indexedDB, 'databases', { configurable: true, value: undefined })
  expect(await cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map(), discardUnreadable: true })).toBe(false)
  expect(pendingSignOutOf(7)).toMatchObject({ session: 's1' })
})

test('a start whose answer comes late never takes a newer login of the account back, nor settles its sign-out', async () => {
  let answer: (() => void) | undefined
  fakeFetch({
    ...signedIn,
    'GET /api/me': () =>
      new Promise((done) => {
        answer = () => done({ body: me })
      }),
  })
  render(<App />)
  await waitFor(() => expect(answer).toBeDefined())
  await new Promise((r) => setTimeout(r, 20))
  await finishUnfinishedSignOuts({ ...me, session: 's2' })
  await activate(7, 'Pat')
  recordSignOut(7, 's2', false, new Map())
  await act(async () => answer?.())
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(pendingSignOutOf(7)).toMatchObject({ session: 's2' })
  await deactivate({ purge: false })
  expect(await cleanUpAfterSignOut(7, 's2', { keep: false, approved: new Map() })).toBe(true)
  expect(lastUser()).toBeNull()
})

test('a start answered 401 leaves a sign-out another tab recorded after it asked to that tab', async () => {
  let answer: (() => void) | undefined
  fakeFetch({
    ...signedIn,
    'GET /api/me': () =>
      new Promise((done) => {
        answer = () => done({ status: 401, body: { error: 'not logged in' } })
      }),
  })
  await activate(7, 'Pat')
  runtimeState().runtime?.engine.pause('manual')
  render(<App />)
  await waitFor(() => expect(answer).toBeDefined())
  await new Promise((r) => setTimeout(r, 20))
  recordSignOut(7, 's2', false, new Map())
  await act(async () => answer?.())
  await screen.findByRole('heading', { name: 'Log in' })
  await new Promise((r) => setTimeout(r, 50))
  expect(pendingSignOutOf(7)).toMatchObject({ session: 's2' })
  expect(lastUser()).toEqual({ id: 7, name: 'Pat' })
  expect(await databases()).toContain('taskto-offline-7')
})

test("a sign-out's deletion the browser keeps blocked gives the account's lock up: the next sign-in opens the database", async () => {
  fakeLocks()
  const overs: (() => void)[] = []
  lockTimes.holdOver = (_ms, over) => {
    overs.push(over)
    return () => {}
  }
  await activate(7, 'Pat')
  runtimeState().runtime?.engine.pause('manual')
  const blocker = await new Promise<IDBDatabase>((resolve, reject) => {
    const request = indexedDB.open('taskto-offline-7')
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
  })
  const deletions = vi.spyOn(indexedDB, 'deleteDatabase')
  let blockedSeen = false
  deletions.mockImplementation(function (this: IDBFactory, name: string) {
    const request = IDBFactory.prototype.deleteDatabase.call(this, name)
    request.addEventListener('blocked', () => {
      blockedSeen = true
    })
    return request
  })
  void cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map() })
  await waitFor(() => expect(blockedSeen).toBe(true))
  for (const over of overs.splice(0)) over()
  await navigator.locks.request('taskto-offline-db:7', async () => {})
  deletions.mockRestore()
  blocker.close()
  const opened = await OfflineStore.open(7)
  opened.close()
  await activate(8, 'Sam')
  expect(runtimeState().runtime?.user).toBe(8)
})

test("waiting for an account's lock is bounded: the database is said to be unavailable, never waited for for good", async () => {
  fakeLocks()
  const waits: AbortController[] = []
  lockTimes.waitOver = () => {
    const wait = new AbortController()
    waits.push(wait)
    return wait.signal
  }
  let release: () => void = () => {}
  void navigator.locks.request('taskto-offline-db:7', () => new Promise<void>((done) => (release = done)))
  const opening = OfflineStore.open(7)
  for (const wait of waits) wait.abort()
  await expect(opening).rejects.toThrow()
  release()
})

test('a newer login that starts while a cleanup opens the database keeps it as it is: nothing is retired or emptied', async () => {
  const before = await OfflineStore.open(7)
  await before.enqueue(queued(), [])
  before.close()
  recordSignOut(7, 's1', false, new Map())
  const real = OfflineStore.open.bind(OfflineStore)
  vi.spyOn(OfflineStore, 'open').mockImplementationOnce(async (user: number) => {
    const opened = await real(user)
    await finishUnfinishedSignOuts({ ...me, session: 's2' })
    return opened
  })
  const [item] = await (async () => {
    const s = await real(7)
    const items = await s.listItems()
    s.close()
    return items
  })()
  const retire = vi.spyOn(OfflineStore.prototype, 'retireOrKeepUnsent')
  await cleanUpAfterSignOut(7, 's1', {
    keep: false,
    approved: new Map([[item.id, { version: item.version, operation: item.operation }]]),
  })
  expect(retire).not.toHaveBeenCalled()
  vi.restoreAllMocks()
  const after = await OfflineStore.open(7)
  expect((await after.listItems()).map((i) => i.id)).toEqual(['item-1'])
  await after.enqueue(queued('newer'), [])
  after.close()
})

test('a cleanup that found the database unreadable is not verified clear, even if it reads a moment later', async () => {
  recordSignOut(7, 's1', false, new Map())
  await activate(7, 'Pat')
  runtimeState().runtime?.engine.pause('manual')
  vi.spyOn(OfflineStore.prototype, 'retireOrKeepUnsent').mockRejectedValueOnce(new Error('read failed once'))
  expect(await cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map() })).toBe(false)
  expect(pendingSignOutOf(7)).toMatchObject({ session: 's1' })
})

test("Back's claim stops when another person signs in while it finishes earlier sign-outs: their drafts and marker stay", async () => {
  fakeFetch({
    ...signedIn,
    'GET /api/me': { body: me },
    'POST /api/logout': { status: 503, body: { error: 'unavailable', code: 'internal' } },
  })
  await signedInAs()
  logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  const marker = JSON.stringify({ id: 8, name: 'Sam' })
  during.claim = () => {
    localStorage.setItem('taskto:drafts-of', '8')
    localStorage.setItem('taskto:builder-start:8:1', 'a draft of the second person')
    localStorage.setItem(LAST, marker)
    window.dispatchEvent(new StorageEvent('storage', { key: LAST, newValue: marker }))
  }
  fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await new Promise((r) => setTimeout(r, 50))
  expect(localStorage.getItem('taskto:drafts-of')).toBe('8')
  expect(localStorage.getItem('taskto:builder-start:8:1')).toBe('a draft of the second person')
  expect(lastUser()).toEqual({ id: 8, name: 'Sam' })
})

test('asking whether a database exists never leaves one behind, even when removing what it made fails', async () => {
  Object.defineProperty(indexedDB, 'databases', { configurable: true, value: undefined })
  const refused = vi.spyOn(indexedDB, 'deleteDatabase').mockImplementation(() => {
    const request = { error: new DOMException('refused', 'UnknownError') } as unknown as IDBOpenDBRequest
    setTimeout(() => request.onerror?.(new Event('error')), 0)
    return request
  })
  expect(await databaseExists(9)).toBe(false)
  refused.mockRestore()
  Reflect.deleteProperty(indexedDB, 'databases')
  expect(await databases()).not.toContain('taskto-offline-9')
})

test("an account's deletion forgets its login on this browser: it shields nothing afterwards", async () => {
  const { forgetAccount } = await import('../auth/forgetAccount')
  const { browserLogin } = await import('./pendingSignOut')
  await finishUnfinishedSignOuts({ ...me, session: 's2' })
  await forgetAccount(7)
  expect(browserLogin()).toBeNull()
})

test('a newer login that starts just as the cleanup begins its transaction keeps the database as it is', async () => {
  const before = await OfflineStore.open(7)
  await before.enqueue(queued(), [])
  const [item] = await before.listItems()
  before.close()
  recordSignOut(7, 's1', false, new Map())
  const real = OfflineStore.prototype.retireOrKeepUnsent
  vi.spyOn(OfflineStore.prototype, 'retireOrKeepUnsent').mockImplementation(async function (
    this: OfflineStore,
    ...args: Parameters<typeof real>
  ) {
    await finishUnfinishedSignOuts({ ...me, session: 's2' })
    return real.apply(this, args)
  })
  await cleanUpAfterSignOut(7, 's1', {
    keep: false,
    approved: new Map([[item.id, { version: item.version, operation: item.operation }]]),
  })
  vi.restoreAllMocks()
  const after = await OfflineStore.open(7)
  expect((await after.listItems()).map((i) => i.id)).toEqual(['item-1'])
  await after.enqueue(queued('newer'), [])
  after.close()
})

test("a stale tab's sign-out never takes away a newer login's decision", async () => {
  fakeFetch({
    ...signedIn,
    'GET /api/me': { body: me },
    'POST /api/logout': { status: 409, body: { error: 'not this login', code: 'session_account_changed' } },
  })
  await signedInAs()
  await finishUnfinishedSignOuts({ ...me, session: 's2' })
  recordSignOut(7, 's2', false, new Map([['item-1', { version: 1, operation: 'op-race-item-1' }]]))
  unansweredSignOut(7, 's2')
  backFromSignOut(7, 's2')
  logOut()
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await new Promise((r) => setTimeout(r, 20))
  expect(pendingSignOuts().find((p) => p.session === 's2')).toMatchObject({
    phase: 'backed',
    approved: [['item-1', { version: 1, operation: 'op-race-item-1' }]],
  })
})

test("a start answered late for an account that isn't the browser's any more claims nothing of the one that is", async () => {
  let answer: (() => void) | undefined
  fakeFetch({
    ...signedIn,
    'GET /api/me': () =>
      new Promise((done) => {
        answer = () => done({ body: me })
      }),
  })
  render(<App />)
  await waitFor(() => expect(answer).toBeDefined())
  const sam = { ...me, user: { id: 8, email: 'sam@example.com', name: 'Sam' }, session: 't1' } as Me
  await finishUnfinishedSignOuts(sam)
  localStorage.setItem('taskto:drafts-of', '8')
  localStorage.setItem('taskto:builder-start:8:1', 'a draft of the second person')
  localStorage.setItem(LAST, JSON.stringify({ id: 8, name: 'Sam' }))
  await act(async () => answer?.())
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await new Promise((r) => setTimeout(r, 20))
  expect(localStorage.getItem('taskto:drafts-of')).toBe('8')
  expect(localStorage.getItem('taskto:builder-start:8:1')).toBe('a draft of the second person')
  expect(browserLogin()).toMatchObject({ user: 8, session: 't1' })
  expect(lastUser()).toEqual({ id: 8, name: 'Sam' })
})

test('a start answered late for a login that has signed out since opens nothing of it', async () => {
  rememberLogin(7, 's1', Date.now() - 1000)
  let answer: (() => void) | undefined
  fakeFetch({
    ...signedIn,
    'GET /api/me': () =>
      new Promise((done) => {
        answer = () => done({ body: me })
      }),
  })
  render(<App />)
  await waitFor(() => expect(answer).toBeDefined())
  recordSignOut(7, 's1', false, new Map())
  expect(await cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map() })).toBe(true)
  await act(async () => answer?.())
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await new Promise((r) => setTimeout(r, 20))
  expect(lastUser()).toBeNull()
  expect(localStorage.getItem('taskto:drafts-of')).toBeNull()
})

test('a start whose earlier sign-outs take a while claims nothing when another person signed in meanwhile', async () => {
  recordSignOut(9, 'u1', false, new Map())
  const real = OfflineStore.open.bind(OfflineStore)
  vi.spyOn(OfflineStore, 'open').mockImplementationOnce(async (user: number) => {
    rememberLogin(8, 't1', Date.now() + 1)
    localStorage.setItem('taskto:drafts-of', '8')
    localStorage.setItem('taskto:builder-start:8:1', 'a draft of the second person')
    return real(user)
  })
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  render(<App />)
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await new Promise((r) => setTimeout(r, 20))
  expect(localStorage.getItem('taskto:drafts-of')).toBe('8')
  expect(localStorage.getItem('taskto:builder-start:8:1')).toBe('a draft of the second person')
  expect(browserLogin()).toMatchObject({ user: 8, session: 't1' })
})

test("a start that isn't stale lets go of an older login's sign-out, even one recorded after it asked", async () => {
  let answer: (() => void) | undefined
  fakeFetch({
    ...signedIn,
    'GET /api/me': () =>
      new Promise((done) => {
        answer = () => done({ body: me })
      }),
  })
  rememberLogin(7, 's2', Date.now() - 1000)
  render(<App />)
  await waitFor(() => expect(answer).toBeDefined())
  recordSignOut(7, 's2', false, new Map())
  await act(async () => answer?.())
  await screen.findByRole('banner')
  expect(pendingSignOuts().find((p) => p.session === 's2')).toBeUndefined()
  expect(signOutPending(7)).toBe(false)
})

test('a start whose login another tab replaced while it took the account up opens nothing for it', async () => {
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  during.activate = () => {
    rememberLogin(7, 's2', Date.now() + 1)
  }
  render(<App />)
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(screen.queryByRole('navigation', { name: 'Sections' })).toBeNull()
  await waitFor(() => expect(runtimeState().runtime).toBeNull())
})

test("a newer login that wins while the cleanup's transaction is deleting leaves the database untouched", async () => {
  await activate(7, 'Pat')
  const runtime = runtimeState().runtime
  runtime?.engine.pause('manual')
  await runtime?.store.enqueue(queued(), [])
  const [item] = (await runtime?.store.listItems()) ?? []
  await deactivate({ purge: false, user: 7 })
  recordSignOut(7, 's1', false, new Map())
  const real = IDBObjectStore.prototype.delete
  let taken = false
  vi.spyOn(IDBObjectStore.prototype, 'delete').mockImplementation(function (
    this: IDBObjectStore,
    key: IDBValidKey | IDBKeyRange,
  ) {
    if (!taken && this.name === 'outbox') {
      taken = true
      rememberLogin(7, 's2', Date.now() + 1)
    }
    return real.call(this, key)
  })
  await cleanUpAfterSignOut(7, 's1', {
    keep: false,
    approved: new Map([[item.id, { version: item.version, operation: item.operation }]]),
  })
  vi.restoreAllMocks()
  expect(taken).toBe(true)
  const after = await OfflineStore.open(7)
  expect((await after.listItems()).map((i) => i.id)).toEqual(['item-1'])
  await after.enqueue(queued('newer'), [])
  after.close()
})

test('a start that another tab signs out while it takes the account up brings back nothing of it', async () => {
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  during.activate = () => {
    rememberLogin(7, 's1', Date.now() - 1)
    forgetLogin(7, 's1')
    localStorage.removeItem(LAST)
  }
  render(<App />)
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await waitFor(() => expect(runtimeState().runtime).toBeNull())
  expect(lastUser()).toBeNull()
  expect(await databases()).not.toContain('taskto-offline-7')
})

test("a start that another person's sign-in overtakes while it takes the account up leaves theirs alone", async () => {
  const theirs = await OfflineStore.open(8)
  theirs.close()
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  during.activate = () => {
    rememberLogin(8, 't1', Date.now() + 1)
    localStorage.setItem(LAST, JSON.stringify({ id: 8, name: 'Sam' }))
  }
  render(<App />)
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await waitFor(() => expect(runtimeState().runtime).toBeNull())
  expect(lastUser()).toEqual({ id: 8, name: 'Sam' })
  expect(await databases()).toContain('taskto-offline-8')
})

test("a start whose login another tab replaced while its time zone was reported never draws the old login's shell", async () => {
  let report: (() => void) | undefined
  const fresh = { ...me, user: { ...me.user, time_zone: '' } } as Me
  fakeFetch({
    ...signedIn,
    'GET /api/me': { body: fresh },
    'PATCH /api/me': () =>
      new Promise((done) => {
        report = () => done({ body: { ...fresh, user: { ...fresh.user, time_zone: 'UTC' } } })
      }),
  })
  render(<App />)
  await waitFor(() => expect(report).toBeDefined())
  rememberLogin(7, 's2', Date.now() + 1)
  await act(async () => report?.())
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(screen.queryByRole('navigation', { name: 'Sections' })).toBeNull()
  await waitFor(() => expect(runtimeState().runtime).toBeNull())
})

test("a start answered late never sets the browser's language for an account that isn't the browser's any more", async () => {
  let answer: (() => void) | undefined
  fakeFetch({
    ...signedIn,
    'GET /api/me': () =>
      new Promise((done) => {
        answer = () => done({ body: { ...me, user: { ...me.user, locale: 'pt-BR' } } })
      }),
  })
  render(<App />)
  await waitFor(() => expect(answer).toBeDefined())
  rememberLogin(8, 't1', Date.now() + 1)
  localStorage.setItem('taskto.locale', 'en')
  await act(async () => answer?.())
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(localStorage.getItem('taskto.locale')).toBe('en')
})

test("a third tab signing the older login out again never makes the first one's late logout clear the newer login's device", async () => {
  await activate(7, 'Pat')
  runtimeState().runtime?.engine.pause('manual')
  await deactivate({ purge: false, user: 7 })
  const clock = vi.spyOn(Date, 'now')
  clock.mockReturnValue(1_000)
  recordSignOut(7, 's1', false, new Map())
  const recorded = pendingSignOuts().find((p) => p.session === 's1')
  clock.mockReturnValue(2_000)
  await finishUnfinishedSignOuts({ ...me, session: 's2' })
  clock.mockReturnValue(3_000)
  recordSignOut(7, 's1', false, new Map())
  clock.mockRestore()
  expect(await cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map(), at: recorded?.at })).toBe(true)
  expect(lastUser()).toEqual({ id: 7, name: 'Pat' })
  expect(await databases()).toContain('taskto-offline-7')
})

test('a start that takes a sign-out up goes on signing in when another tab went back from it meanwhile', async () => {
  recordSignOut(7, 's1', false, new Map())
  recordSignOut(9, 'u1', false, new Map())
  const calls = fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  const real = OfflineStore.open.bind(OfflineStore)
  vi.spyOn(OfflineStore, 'open').mockImplementationOnce(async (user: number) => {
    backFromSignOut(7, 's1')
    return real(user)
  })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  expect(calls.some((c) => c.path === '/api/logout')).toBe(false)
})

test("a database open that outlasted the account's lock never deletes what another tab made of it since", async () => {
  const store = await OfflineStore.open(7)
  await store.retireOrKeepUnsent()
  store.close()
  fakeLocks()
  lockTimes.holdOver = (_ms, over) => {
    queueMicrotask(over)
    return () => {}
  }
  await expect(OfflineStore.open(7)).rejects.toThrow()
  await new Promise((r) => setTimeout(r, 0))
  await new Promise((r) => setTimeout(r, 0))
  const raw = await new Promise<IDBDatabase>((resolve, reject) => {
    const request = indexedDB.open('taskto-offline-7')
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
  })
  const retired = await new Promise((resolve) => {
    const get = raw.transaction('meta').objectStore('meta').get('retired')
    get.onsuccess = () => resolve(get.result)
  })
  raw.close()
  expect(retired).toBeDefined()
})

test("a start whose login another tab replaces while it opens the account's database lets go of all it took", async () => {
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  const real = OfflineStore.open.bind(OfflineStore)
  vi.spyOn(OfflineStore, 'open').mockImplementationOnce(async (user: number) => {
    rememberLogin(7, 's2', Date.now() + 1)
    return real(user)
  })
  render(<App />)
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await waitFor(() => expect(runtimeState().runtime).toBeNull())
})

test("a sign-out's deletion whose hold on the account's lock ran out deletes nothing", async () => {
  const store = await OfflineStore.open(7)
  await store.retireOrKeepUnsent()
  store.close()
  fakeLocks()
  lockTimes.holdOver = (_ms, over) => {
    queueMicrotask(over)
    return () => {}
  }
  await expect(OfflineStore.deleteRetired(7)).rejects.toThrow()
  await new Promise((r) => setTimeout(r, 0))
  await new Promise((r) => setTimeout(r, 0))
  expect(await databases()).toContain('taskto-offline-7')
})

test("a start that another person's sign-in overtakes while its time zone is reported never sets its language", async () => {
  let report: (() => void) | undefined
  const fresh = { ...me, user: { ...me.user, time_zone: '', locale: 'pt-BR' } } as Me
  fakeFetch({
    ...signedIn,
    'GET /api/me': { body: fresh },
    'PATCH /api/me': () =>
      new Promise((done) => {
        report = () => done({ body: { ...fresh, user: { ...fresh.user, time_zone: 'UTC' } } })
      }),
  })
  render(<App />)
  await waitFor(() => expect(report).toBeDefined())
  rememberLogin(8, 't1', Date.now() + 1)
  localStorage.setItem('taskto.locale', 'en')
  await act(async () => report?.())
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(localStorage.getItem('taskto.locale')).toBe('en')
})

test('a start that takes a sign-out up signs in instead when another tab went back from it while its language was set', async () => {
  recordSignOut(7, 's1', false, new Map())
  const calls = fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  let release: () => void = () => {}
  hold.reached = false
  hold.preferences = new Promise((done) => {
    release = () => {
      hold.preferences = null
      done()
    }
  })
  render(<App />)
  await waitFor(() => expect(hold.reached).toBe(true))
  backFromSignOut(7, 's1')
  await act(async () => release())
  await screen.findByRole('navigation', { name: 'Sections' })
  expect(calls.some((c) => c.path === '/api/logout')).toBe(false)
})

async function samsDataHere() {
  const sams = await OfflineStore.open(8)
  await sams.putDownload(samDownload)
  sams.close()
  localStorage.setItem('taskto:offline-users', JSON.stringify([8]))
}
const samDownload = {
  workspace: 1,
  space: 2,
  app: 1,
  key: '1/2/1',
  user: 8,
  name: 'Expenses',
  release: 3,
  definition: { definition_version: 1, requires: [], collection: { fields: [] }, actions: {} },
  messages: [],
  scopeOf: 'all',
  records: 0,
  total: 0,
  truncated: false,
  canInsert: true,
  capturedAt: '2026-11-02T10:00:00.000Z',
  bytes: 1,
} as never

test("a start's sweep of another account's data stops when that person signs in again meanwhile", async () => {
  await samsDataHere()
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  const real = OfflineStore.open.bind(OfflineStore)
  vi.spyOn(OfflineStore, 'open').mockImplementation(async (user: number) => {
    const opened = await real(user)
    if (user === 8 && lastUser()?.id !== 8) {
      rememberLogin(8, 't1', Date.now() + 1)
      localStorage.setItem(LAST, JSON.stringify({ id: 8, name: 'Sam' }))
    }
    return opened
  })
  const retire = vi.spyOn(OfflineStore.prototype, 'retireOrKeepUnsent')
  render(<App />)
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await waitFor(() => expect(runtimeState().runtime).toBeNull())
  expect(retire.mock.contexts.some((store) => (store as OfflineStore).user === 8)).toBe(false)
  vi.restoreAllMocks()
  const sams = await OfflineStore.open(8)
  expect(await sams.listDownloads()).toHaveLength(1)
  sams.close()
})

test("a start that another tab signs out while it sweeps creates nothing of the account's", async () => {
  await samsDataHere()
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  const real = OfflineStore.open.bind(OfflineStore)
  vi.spyOn(OfflineStore, 'open').mockImplementation(async (user: number) => {
    if (user === 8) forgetLogin(7, 's1')
    return real(user)
  })
  render(<App />)
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await waitFor(() => expect(runtimeState().runtime).toBeNull())
  vi.restoreAllMocks()
  expect(await databases()).not.toContain('taskto-offline-7')
})

test('a start whose login another tab replaces while it opens the database starts no queue or writer for it', async () => {
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  const { OfflineWriter } = await import('./writer')
  const writer = vi.spyOn(OfflineWriter.prototype, 'start')
  const real = OfflineStore.open.bind(OfflineStore)
  vi.spyOn(OfflineStore, 'open').mockImplementationOnce(async (user: number) => {
    rememberLogin(7, 's2', Date.now() + 1)
    return real(user)
  })
  render(<App />)
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(writer).not.toHaveBeenCalled()
})

test('a database open whose lock ran out while it deleted a retired database never makes it again', async () => {
  const store = await OfflineStore.open(7)
  await store.retireOrKeepUnsent()
  store.close()
  fakeLocks()
  const overs: (() => void)[] = []
  lockTimes.holdOver = (_ms, over) => {
    overs.push(over)
    return () => {}
  }
  const real = IDBFactory.prototype.deleteDatabase
  vi.spyOn(indexedDB, 'deleteDatabase').mockImplementation(function (this: IDBFactory, name: string) {
    for (const over of overs.splice(0)) over()
    return real.call(this, name)
  })
  await expect(OfflineStore.open(7)).rejects.toThrow()
  await new Promise((r) => setTimeout(r, 0))
  await new Promise((r) => setTimeout(r, 0))
  vi.restoreAllMocks()
  expect(await databases()).not.toContain('taskto-offline-7')
})

test('a cleanup whose verification a newer login of the account overtook is over, not failed', async () => {
  recordSignOut(7, 's1', false, new Map(), true)
  await finishUnfinishedSignOuts(me)
  recordSignOut(7, 's1', false, new Map(), true)
  await activate(7, 'Pat')
  runtimeState().runtime?.engine.pause('manual')
  await deactivate({ purge: false, user: 7 })
  vi.spyOn(OfflineStore.prototype, 'retireOrKeepUnsent').mockRejectedValue(new Error('read failed'))
  vi.spyOn(indexedDB, 'deleteDatabase').mockImplementation(() => {
    const request = { error: new DOMException('refused', 'UnknownError') } as unknown as IDBOpenDBRequest
    setTimeout(() => request.onerror?.(new Event('error')), 0)
    return request
  })
  const listing = indexedDB.databases.bind(indexedDB)
  vi.spyOn(indexedDB, 'databases').mockImplementation(async () => {
    rememberLogin(7, 's2', Date.now() + 1)
    return listing()
  })
  expect(await cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map(), discardUnreadable: true })).toBe(true)
  expect(pendingSignOuts().find((p) => p.session === 's1')).toBeUndefined()
})

test('a start that takes a sign-out up opens nothing when another login started while its language was set', async () => {
  recordSignOut(7, 's1', false, new Map())
  const calls = fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  let release: () => void = () => {}
  hold.reached = false
  hold.preferences = new Promise((done) => {
    release = () => {
      hold.preferences = null
      done()
    }
  })
  const opens = vi.spyOn(OfflineStore, 'open')
  render(<App />)
  await waitFor(() => expect(hold.reached).toBe(true))
  rememberLogin(8, 't1', Date.now() + 1)
  await act(async () => release())
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(calls.some((c) => c.path === '/api/logout')).toBe(false)
  expect(opens.mock.calls.some(([user]) => user === 7)).toBe(false)
})

test('an answer asked in the same moment a login was seen to end is not taken for that login', () => {
  rememberLogin(7, 's1', 1_000)
  const clock = vi.spyOn(Date, 'now').mockReturnValue(2_000)
  forgetLogin(7, 's1')
  clock.mockRestore()
  expect(rememberLogin(7, 's1', 2_000)).toBe('stale')
})

test("the time zone's report names the account it is for", async () => {
  const fresh = { ...me, user: { ...me.user, time_zone: '' } } as Me
  const calls = fakeFetch({
    ...signedIn,
    'GET /api/me': { body: fresh },
    'PATCH /api/me': { body: { ...fresh, user: { ...fresh.user, time_zone: 'UTC' } } },
  })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  const patch = calls.find((c) => c.method === 'PATCH' && c.path === '/api/me')
  expect(patch?.headers['x-taskto-expected-user']).toBe('7')
})

test("a start's sweep whose transaction the other person's sign-in overtakes removes nothing of theirs", async () => {
  await samsDataHere()
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  const real = IDBObjectStore.prototype.delete
  let taken = false
  vi.spyOn(IDBObjectStore.prototype, 'delete').mockImplementation(function (
    this: IDBObjectStore,
    key: IDBValidKey | IDBKeyRange,
  ) {
    if (this.name === 'downloads' && this.transaction.db.name === 'taskto-offline-8' && !taken) {
      taken = true
      rememberLogin(8, 't1', Date.now() + 1)
      localStorage.setItem(LAST, JSON.stringify({ id: 8, name: 'Sam' }))
    }
    return real.call(this, key)
  })
  render(<App />)
  await waitFor(() => expect(taken).toBe(true))
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await waitFor(() => expect(runtimeState().runtime).toBeNull())
  vi.restoreAllMocks()
  const sams = await OfflineStore.open(8)
  expect(await sams.listDownloads()).toHaveLength(1)
  sams.close()
})

test("a start's sweep never deletes what the other person saved after its database was retired", async () => {
  await samsDataHere()
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  let saved = false
  const retire = OfflineStore.prototype.retireOrKeepUnsent
  vi.spyOn(OfflineStore.prototype, 'retireOrKeepUnsent').mockImplementation(async function (
    this: OfflineStore,
    ...args: Parameters<typeof retire>
  ) {
    const out = await retire.apply(this, args)
    if (this.user === 8 && !saved) {
      rememberLogin(8, 't1', Date.now() + 1)
      localStorage.setItem(LAST, JSON.stringify({ id: 8, name: 'Sam' }))
      const theirs = await OfflineStore.open(8)
      await theirs.enqueue({ ...queued('sams'), user: 8 }, [])
      theirs.close()
      saved = true
    }
    return out
  })
  render(<App />)
  await waitFor(() => expect(saved).toBe(true))
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await waitFor(() => expect(runtimeState().runtime).toBeNull())
  vi.restoreAllMocks()
  const sams = await OfflineStore.open(8)
  expect((await sams.listItems()).map((i) => i.id)).toEqual(['sams'])
  sams.close()
})

test('a sign-in in a tab whose runtime is still open lets it go when another login overtakes it', async () => {
  fakeFetch({
    ...signedIn,
    'GET /api/me': { body: me },
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/login': { body: { ...me, session: 's3' } },
  })
  await signedInAs()
  await act(async () => {})
  fakeFetch({
    ...signedIn,
    'GET /api/me': { status: 401, body: { error: 'not logged in' } },
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/login': { body: { ...me, session: 's3' } },
  })
  await act(async () => {
    await api('/api/me').catch(() => {})
  })
  await screen.findByLabelText('Password')
  expect(runtimeState().runtime?.user).toBe(7)
  during.activate = () => {
    rememberLogin(7, 's4', Date.now() + 1)
  }
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'pat@example.com' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await waitFor(() => expect(runtimeState().runtime).toBeNull())
})

test('a start whose login another person overtakes just after its earlier sign-outs claims none of their drafts', async () => {
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  during.claimed = () => {
    rememberLogin(8, 't1', Date.now() + 1)
    localStorage.setItem('taskto:drafts-of', '8')
    localStorage.setItem('taskto:builder-start:8:1', 'a draft of the second person')
  }
  render(<App />)
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(localStorage.getItem('taskto:drafts-of')).toBe('8')
  expect(localStorage.getItem('taskto:builder-start:8:1')).toBe('a draft of the second person')
})

test("an older login's sign-out recorded while the newer login's start waited for its answer holds nothing of it", async () => {
  rememberLogin(7, 's1', Date.now() - 10)
  recordSignOut(7, 's1', false, new Map(), false, Date.now() + 60_000)
  unansweredSignOut(7, 's1')
  fakeFetch({ ...signedIn, 'GET /api/me': { body: { ...me, session: 's2' } } })
  await signedInAs()
  expect(signOutPending(7)).toBe(false)
  expect(pendingSignOuts()).toEqual([])
  cleanup()
  await deactivate({ purge: false, user: 7 })
  expect((await activate(7)).runtime?.user).toBe(7)
})

test.each([
  ['a newer login of the same person', 7, 's4', 'Patricia'],
  ['another person', 8, 't1', 'Sam'],
])(
  'a sign-in that takes up the runtime still open here writes no marker and starts nothing once %s signed in',
  async (_, user, session, name) => {
    fakeFetch({
      ...signedIn,
      'GET /api/me': { body: me },
      'GET /api/login/methods': { body: { password: true, sso: [] } },
      'POST /api/login': { body: { ...me, session: 's3' } },
    })
    await signedInAs()
    await act(async () => {})
    fakeFetch({
      ...signedIn,
      'GET /api/me': { status: 401, body: { error: 'not logged in' } },
      'GET /api/login/methods': { body: { password: true, sso: [] } },
      'POST /api/login': { body: { ...me, session: 's3' } },
    })
    await act(async () => {
      await api('/api/me').catch(() => {})
    })
    await screen.findByLabelText('Password')
    const runtime = runtimeState().runtime
    if (runtime?.user !== 7) throw new Error('no runtime')
    const resume = vi.spyOn(runtime.engine, 'resume')
    during.activate = () => {
      rememberLogin(user, session, Date.now() + 1)
      localStorage.setItem(LAST, JSON.stringify({ id: user, name }))
    }
    fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'pat@example.com' } })
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
    fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
    expect(await screen.findByText('The account changed')).toBeTruthy()
    expect(lastUser()).toEqual({ id: user, name })
    expect(resume).not.toHaveBeenCalled()
  },
)

test("a sign-in overtaken while it takes the account up never sets the browser's language", async () => {
  fakeFetch({
    ...signedIn,
    'GET /api/me': { status: 401, body: { error: 'not logged in' } },
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/login': { body: { ...me, user: { ...me.user, locale: 'pt-BR' } } },
  })
  render(<App />)
  await screen.findByLabelText('Password')
  during.claim = () => {
    rememberLogin(8, 't1', Date.now() + 60_000)
  }
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'pat@example.com' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(localStorage.getItem('taskto.locale')).toBeNull()
})

test("a start waiting for the account's lock behind a sign-out never makes the database again once its login is over", async () => {
  const names = fakeLocks()
  let release: () => void = () => {}
  const held = navigator.locks.request('taskto-offline-db:7', () => new Promise<void>((done) => (release = done)))
  let current = true
  const started = activate(7, 'Pat', () => current)
  await waitFor(() => expect(names.filter((n) => n === 'taskto-offline-db:7')).toHaveLength(2))
  current = false
  const opens = vi.spyOn(indexedDB, 'open')
  release()
  await held
  expect((await started).runtime).toBeNull()
  expect(opens.mock.calls.some(([name]) => name === 'taskto-offline-7')).toBe(false)
  vi.restoreAllMocks()
  expect(await databases()).not.toContain('taskto-offline-7')
})

function endsWhileOpening(): { current: () => boolean } {
  let current = true
  const real = IDBFactory.prototype.open
  vi.spyOn(indexedDB, 'open').mockImplementation(function (this: IDBFactory, name: string, version?: number) {
    if (name === 'taskto-offline-7') current = false
    return real.call(this, name, version)
  })
  return { current: () => current }
}

test("a database a start made for a login that ended meanwhile is deleted again before the account's lock goes", async () => {
  fakeLocks()
  const login = endsWhileOpening()
  expect(await activate(7, 'Pat', login.current)).toMatchObject({ runtime: null, problem: 'account' })
  vi.restoreAllMocks()
  expect(await databases()).not.toContain('taskto-offline-7')
})

test('a start whose login ended while it opened the database leaves what was already in it', async () => {
  const store = await OfflineStore.open(7)
  await store.enqueue(queued('kept'), [])
  store.close()
  fakeLocks()
  const login = endsWhileOpening()
  expect(await activate(7, 'Pat', login.current)).toMatchObject({ runtime: null, problem: 'account' })
  vi.restoreAllMocks()
  const after = await OfflineStore.open(7)
  expect((await after.listItems()).map((i) => i.id)).toEqual(['kept'])
  after.close()
})

test("a sign-out's check that the device is clear never makes the database it looks for", async () => {
  recordSignOut(7, 's1', false, new Map())
  vi.spyOn(indexedDB, 'databases').mockResolvedValue([{ name: 'taskto-offline-7', version: 1 }])
  expect(await cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map() })).toBe(true)
  vi.restoreAllMocks()
  expect(await databases()).not.toContain('taskto-offline-7')
})

test("discarding other accounts' records stops at one that signed in again meanwhile", async () => {
  for (const id of [8, 9]) {
    const theirs = await OfflineStore.open(id)
    await theirs.enqueue({ ...queued(`of-${id}`), user: id }, [])
    theirs.close()
  }
  localStorage.setItem('taskto:offline-users', JSON.stringify([8, 9]))
  const state = await activate(7, 'Pat')
  expect(state.foreign).toEqual({ users: [8, 9], count: 2 })
  runtimeState().runtime?.engine.pause('manual')
  const real = IDBFactory.prototype.deleteDatabase
  vi.spyOn(indexedDB, 'deleteDatabase').mockImplementation(function (this: IDBFactory, name: string) {
    if (name === 'taskto-offline-8') localStorage.setItem(LAST, JSON.stringify({ id: 9, name: 'Kim' }))
    return real.call(this, name)
  })
  await discardForeign()
  vi.restoreAllMocks()
  expect(await databases()).not.toContain('taskto-offline-8')
  const kims = await OfflineStore.open(9)
  expect((await kims.listItems()).map((i) => i.id)).toEqual(['of-9'])
  kims.close()
})

test.each([
  ['its deletion of the retired database', 'deleteDatabase'],
  ['its making of it again', 'open'],
] as const)('a start whose login ended during %s leaves no database made again', async (_, when) => {
  const retired = await OfflineStore.open(7)
  await retired.retireOrKeepUnsent()
  retired.close()
  fakeLocks()
  let current = true
  let deleted = false
  let opensAfter = 0
  const realOpen = IDBFactory.prototype.open
  const realDelete = IDBFactory.prototype.deleteDatabase
  vi.spyOn(indexedDB, 'deleteDatabase').mockImplementation(function (this: IDBFactory, name: string) {
    if (name === 'taskto-offline-7' && !deleted) {
      deleted = true
      if (when === 'deleteDatabase') current = false
    }
    return realDelete.call(this, name)
  })
  vi.spyOn(indexedDB, 'open').mockImplementation(function (this: IDBFactory, name: string, version?: number) {
    if (name === 'taskto-offline-7' && deleted) {
      opensAfter++
      if (when === 'open') current = false
    }
    return realOpen.call(this, name, version)
  })
  expect((await activate(7, 'Pat', () => current)).runtime).toBeNull()
  vi.restoreAllMocks()
  if (when === 'deleteDatabase') expect(opensAfter).toBe(0)
  expect(await databases()).not.toContain('taskto-offline-7')
})

test("a start's sweep makes no database for another account whose login it was overtaken in", async () => {
  localStorage.setItem('taskto:offline-users', JSON.stringify([8]))
  fakeLocks()
  let current = true
  const real = IDBFactory.prototype.open
  vi.spyOn(indexedDB, 'open').mockImplementation(function (this: IDBFactory, name: string, version?: number) {
    if (name === 'taskto-offline-8') current = false
    return real.call(this, name, version)
  })
  expect((await activate(7, 'Pat', () => current)).runtime).toBeNull()
  vi.restoreAllMocks()
  expect(await databases()).not.toContain('taskto-offline-8')
})

function supersededAfterOpening() {
  let asked = 0
  return () => ++asked < 2
}

test('without Web Locks, an open that is superseded after it made the database leaves it: another tab may have written to it', async () => {
  await expect(OfflineStore.open(7, { still: supersededAfterOpening() })).rejects.toThrow()
  expect(await databases()).toContain('taskto-offline-7')
})

test('with Web Locks, an open that is superseded after it made the database removes it again', async () => {
  fakeLocks()
  await expect(OfflineStore.open(7, { still: supersededAfterOpening() })).rejects.toThrow()
  expect(await databases()).not.toContain('taskto-offline-7')
})
