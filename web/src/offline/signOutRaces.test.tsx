// @vitest-environment jsdom
//
// A sign-out racing what other tabs do meanwhile, on a browser two people share: every step after a
// wait is taken only for the login it is for, and only while that is still so. A login that is over
// never shields the device; a newer login's records, drafts and marker are never touched; and a
// cleanup that couldn't be verified is said and finished later, never forgotten.

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
import { activate, deactivate, lastUser, runtimeState } from './runtime'
import { databaseExists, lockTimes, OfflineStore, type OutboxItem } from './store'

// Something can be made to happen as a start (or Back) takes the account up, before it is claimed.
const during = vi.hoisted(() => ({ claim: null as (() => void) | null, activate: null as (() => void) | null }))
vi.mock('./runtime', async (original) => {
  const real = await original<typeof import('./runtime')>()
  return {
    ...real,
    // Something can be made to happen just as an account's offline data is taken up (after the
    // claim's own checks, before the account is made the browser's).
    activate: async (...args: Parameters<typeof real.activate>) => {
      const hook = during.activate
      during.activate = null
      hook?.()
      return real.activate(...args)
    },
  }
})
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
    finishUnfinishedSignOuts: async (...args: Parameters<typeof real.finishUnfinishedSignOuts>) => {
      const hook = during.claim
      during.claim = null
      hook?.()
      return real.finishUnfinishedSignOuts(...args)
    },
  }
})

// Setting the page's language can be held, to act while Back waits for it.
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

// A browser's Web Locks, one holder per name at a time, with the names asked for.
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
        // As the browser's: a waiter whose signal aborts before its turn gives up (the turn passes on);
        // once the lock is granted, the signal is ignored.
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

// "Couldn't sign out" (the server refused), with the page's language held for Back's next step.
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
  // Back has its answer from the server and waits on the language now.
  await waitFor(() => expect(hold.reached).toBe(true))
  return release
}

test('a newer login that is over shields nothing: Try again, answered 401 for it, clears the device', async () => {
  let logout: { status: number; body: unknown } = { status: 503, body: { error: 'unavailable', code: 'internal' } }
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me }, 'POST /api/logout': () => logout })
  await signedInAs()
  logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  // Another tab starts with a newer login of the same person (its start remembers it), and that
  // login ends: the browser's cookie is its, so Try again is answered 401.
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
  // This tab's login, the newest, ends.
  fakeFetch({ ...signedIn, 'GET /api/me': { status: 401, body: { error: 'not logged in' } } })
  await act(async () => {
    await api('/api/me').catch(() => {})
  })
  await screen.findByRole('heading', { name: 'Log in' })
  // The older login's sign-out (from a tab that closed) is finished now.
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
    // The account starts in another tab right after the database was retired: it is made again,
    // empty, and a record is saved in it.
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
  // The browser refuses to delete it.
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
  // It couldn't be read when the cleanup asked (so it isn't retired), and it can be now.
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
  // Another tab, signed in again as the same person, is signing that newer login out.
  recordSignOut(7, 's2', false, new Map())
  await cleaning
  expect(pendingSignOutOf(7)).toMatchObject({ session: 's2' })
})

test('Back stops before starting the app again when another tab finished the sign-out meanwhile', async () => {
  const release = await backWithPreferencesHeld()
  // The other tab's cleanup forgets the account: nobody is signed in now.
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
  // The second person signs in in another tab, and begins a draft.
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
  // The app has started again for the login (its offline data claimed); its time zone is on its way.
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
  // The other tab's cleanup forgets the account just as Back takes its offline data up again.
  during.activate = () => {
    localStorage.removeItem(LAST)
    window.dispatchEvent(new StorageEvent('storage', { key: LAST, newValue: null }))
  }
  fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
  await screen.findByRole('heading', { name: 'Log in' })
  // The claim wrote the account back as the browser's: it is taken away again.
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
  // Meanwhile the same person signs in again in another tab, with a newer login, downloads an app's
  // data there, and begins signing that login out.
  await finishUnfinishedSignOuts({ ...me, session: 's2' })
  await activate(7, 'Pat')
  recordSignOut(7, 's2', false, new Map())
  // This tab's start is answered only now, for the login the cookie held when it asked: older than
  // what the browser knows, so it opens nothing, and the page is to be reloaded.
  await act(async () => answer?.())
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(pendingSignOutOf(7)).toMatchObject({ session: 's2' })
  // The newer login's sign-out is finished: the device is cleared, not shielded by the older login.
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
  // Another tab, signed in as Pat, begins signing out: its logout isn't answered yet.
  recordSignOut(7, 's2', false, new Map())
  // This tab's start is answered now: the login its cookie held when it asked is over.
  await act(async () => answer?.())
  await screen.findByRole('heading', { name: 'Log in' })
  await new Promise((r) => setTimeout(r, 50))
  // That sign-out is the other tab's to finish (or the next start's): nothing of Pat's is touched.
  expect(pendingSignOutOf(7)).toMatchObject({ session: 's2' })
  expect(lastUser()).toEqual({ id: 7, name: 'Pat' })
  expect(await databases()).toContain('taskto-offline-7')
})

test("a sign-out's deletion the browser keeps blocked gives the account's lock up: the next sign-in opens the database", async () => {
  fakeLocks()
  // The holder's time is up when the test says so.
  const overs: (() => void)[] = []
  lockTimes.holdOver = (_ms, over) => {
    overs.push(over)
    return () => {}
  }
  await activate(7, 'Pat')
  runtimeState().runtime?.engine.pause('manual')
  // Another connection to the database that never closes when asked (a frozen tab): the deletion
  // is blocked by it.
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
  // The holder's time is up: the account's lock is given back while the deletion is still blocked
  // (the request is granted; were it not, the test would not end).
  for (const over of overs.splice(0)) over()
  await navigator.locks.request('taskto-offline-db:7', async () => {})
  // The other connection closes: the deletion finishes, and the next sign-in on this browser, the
  // same person's or someone else's, goes on.
  deletions.mockRestore()
  blocker.close()
  const opened = await OfflineStore.open(7)
  opened.close()
  await activate(8, 'Sam')
  expect(runtimeState().runtime?.user).toBe(8)
})

test("waiting for an account's lock is bounded: the database is said to be unavailable, never waited for for good", async () => {
  fakeLocks()
  // The wait is over when the test says so.
  const waits: AbortController[] = []
  lockTimes.waitOver = () => {
    const wait = new AbortController()
    waits.push(wait)
    return wait.signal
  }
  let release: () => void = () => {}
  // Something holds the account's lock (a deletion the browser is slow with).
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
    // The same person signs in again in another tab while the cleanup waited for the database.
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
  // Nothing was even begun on it.
  expect(retire).not.toHaveBeenCalled()
  vi.restoreAllMocks()
  const after = await OfflineStore.open(7)
  expect((await after.listItems()).map((i) => i.id)).toEqual(['item-1'])
  // Still the newer login's to save in.
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
    // The second person signs in in another tab, and begins a draft.
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
    // The same person signs in again in another tab right as the transaction starts.
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
    // The browser's login is the newer one: the stale tab's logout is refused.
    'POST /api/logout': { status: 409, body: { error: 'not this login', code: 'session_account_changed' } },
  })
  await signedInAs()
  // Another tab signed the same person in again, chose to discard a record when signing that newer
  // login out, got no answer, and went back.
  await finishUnfinishedSignOuts({ ...me, session: 's2' })
  recordSignOut(7, 's2', false, new Map([['item-1', { version: 1, operation: 'op-race-item-1' }]]))
  unansweredSignOut(7, 's2')
  backFromSignOut(7, 's2')
  // This stale tab, still on the older login, signs out.
  logOut()
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await new Promise((r) => setTimeout(r, 20))
  // The newer login's decision is still there, for its late logout to finish.
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
  // Meanwhile another person signs in in another tab, and begins a draft there.
  const sam = { ...me, user: { id: 8, email: 'sam@example.com', name: 'Sam' }, session: 't1' } as Me
  await finishUnfinishedSignOuts(sam)
  localStorage.setItem('taskto:drafts-of', '8')
  localStorage.setItem('taskto:builder-start:8:1', 'a draft of the second person')
  localStorage.setItem(LAST, JSON.stringify({ id: 8, name: 'Sam' }))
  // This tab's start is answered only now, for the account the cookie held when it asked.
  await act(async () => answer?.())
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await new Promise((r) => setTimeout(r, 20))
  expect(localStorage.getItem('taskto:drafts-of')).toBe('8')
  expect(localStorage.getItem('taskto:builder-start:8:1')).toBe('a draft of the second person')
  expect(browserLogin()).toMatchObject({ user: 8, session: 't1' })
  expect(lastUser()).toEqual({ id: 8, name: 'Sam' })
})

test('a start answered late for a login that has signed out since opens nothing of it', async () => {
  // Another tab has this login.
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
  // That tab signs out, and its logout is confirmed: the login is over.
  recordSignOut(7, 's1', false, new Map())
  expect(await cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map() })).toBe(true)
  // This tab's start is answered only now, for the login its cookie held when it asked.
  await act(async () => answer?.())
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await new Promise((r) => setTimeout(r, 20))
  expect(lastUser()).toBeNull()
  expect(localStorage.getItem('taskto:drafts-of')).toBeNull()
})

test('a start whose earlier sign-outs take a while claims nothing when another person signed in meanwhile', async () => {
  // A sign-out of someone else's left unfinished on this browser: the start finishes it first.
  recordSignOut(9, 'u1', false, new Map())
  const real = OfflineStore.open.bind(OfflineStore)
  vi.spyOn(OfflineStore, 'open').mockImplementationOnce(async (user: number) => {
    // While that cleanup waits on the database, the second person signs in in another tab.
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

test("a start that isn't stale still leaves a sign-out another login recorded after it asked", async () => {
  let answer: (() => void) | undefined
  fakeFetch({
    ...signedIn,
    'GET /api/me': () =>
      new Promise((done) => {
        answer = () => done({ body: me })
      }),
  })
  // Another tab asked who is signed in before this one did, and learned of a newer login.
  rememberLogin(7, 's2', Date.now() - 1000)
  render(<App />)
  await waitFor(() => expect(answer).toBeDefined())
  // That tab begins signing that login out while this start waits for its answer.
  recordSignOut(7, 's2', false, new Map())
  await act(async () => answer?.())
  await screen.findByRole('banner')
  // Recorded after this start asked: that login's to finish, not this start's to drop.
  expect(pendingSignOuts().find((p) => p.session === 's2')).toBeTruthy()
})

test('a start whose login another tab replaced while it took the account up opens nothing for it', async () => {
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  // While the start takes the account's offline data up, the same person signs in again in another
  // tab: a newer login is the browser's.
  during.activate = () => {
    rememberLogin(7, 's2', Date.now() + 1)
  }
  render(<App />)
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(screen.queryByRole('navigation', { name: 'Sections' })).toBeNull()
  // Nothing of the account is left running for the newer login to wait on: no open database, no
  // queue, no writer's lock.
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
  // The same person signs in again in another tab just as the transaction makes its first deletion.
  const real = IDBObjectStore.prototype.delete
  let taken = false
  vi.spyOn(IDBObjectStore.prototype, 'delete').mockImplementation(function (
    this: IDBObjectStore,
    key: IDBValidKey | IDBKeyRange,
  ) {
    // The retirement's own deletions: the queue's records, after its check of what is unsent.
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
  // Nothing was deleted, nothing retired: the newer login's to use.
  const after = await OfflineStore.open(7)
  expect((await after.listItems()).map((i) => i.id)).toEqual(['item-1'])
  await after.enqueue(queued('newer'), [])
  after.close()
})

test('a start that another tab signs out while it takes the account up brings back nothing of it', async () => {
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  // Just as the start takes the account's offline data up, another tab finishes this login's sign-out:
  // the login is over, the marker gone.
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
  // The second person's offline data is on this browser.
  const theirs = await OfflineStore.open(8)
  theirs.close()
  fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  during.activate = () => {
    // The second person signs in in another tab, just as this start takes its account up.
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
  // The same person signs in again in another tab meanwhile.
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
  // Another person signs in in another tab meanwhile, with a language of their own.
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
  // The first tab signs the older login out (its logout is on its way).
  clock.mockReturnValue(1_000)
  recordSignOut(7, 's1', false, new Map())
  const recorded = pendingSignOuts().find((p) => p.session === 's1')
  // The same person signs in again in a second tab, which settles the older login's record.
  clock.mockReturnValue(2_000)
  await finishUnfinishedSignOuts({ ...me, session: 's2' })
  // A third tab, still on the older login, signs it out again.
  clock.mockReturnValue(3_000)
  recordSignOut(7, 's1', false, new Map())
  clock.mockRestore()
  // The first tab's logout is confirmed now (its request carried the older login's cookie).
  expect(await cleanUpAfterSignOut(7, 's1', { keep: false, approved: new Map(), at: recorded?.at })).toBe(true)
  // The newer login's data is untouched.
  expect(lastUser()).toEqual({ id: 7, name: 'Pat' })
  expect(await databases()).toContain('taskto-offline-7')
})

test('a start that takes a sign-out up goes on signing in when another tab went back from it meanwhile', async () => {
  // This login's sign-out, from a tab that closed; and someone else's, which the start finishes first.
  recordSignOut(7, 's1', false, new Map())
  recordSignOut(9, 'u1', false, new Map())
  const calls = fakeFetch({ ...signedIn, 'GET /api/me': { body: me } })
  const real = OfflineStore.open.bind(OfflineStore)
  vi.spyOn(OfflineStore, 'open').mockImplementationOnce(async (user: number) => {
    // While that other cleanup waits, another tab of this login goes back from its sign-out.
    backFromSignOut(7, 's1')
    return real(user)
  })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  expect(calls.some((c) => c.path === '/api/logout')).toBe(false)
})

test("a database open that outlasted the account's lock never deletes what another tab made of it since", async () => {
  // A database a sign-out retired (emptied and marked), not yet deleted.
  const store = await OfflineStore.open(7)
  await store.retireOrKeepUnsent()
  store.close()
  fakeLocks()
  // The holder's time is up as soon as it begins: the lock is let go while the open goes on.
  lockTimes.holdOver = (_ms, over) => {
    queueMicrotask(over)
    return () => {}
  }
  await expect(OfflineStore.open(7)).rejects.toThrow()
  // What it went on doing after that: it must not have deleted the database it no longer held.
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
    // The same person signs in again in another tab while this start opens the database.
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
  // The second person signs in in another tab meanwhile, with a language of their own.
  rememberLogin(8, 't1', Date.now() + 1)
  localStorage.setItem('taskto.locale', 'en')
  await act(async () => report?.())
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(localStorage.getItem('taskto.locale')).toBe('en')
})
