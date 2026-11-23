// The offline machinery of the signed-in user: their database and the engine that sends their
// queue, claimed when someone signs in and let go when they sign out.
//
// Like the drafts (uiState.ts), what is kept for offline use belongs to one account at a time, and a
// database's name is no boundary: when another account signs in, what the last one kept is dealt
// with at once. `activate(user)` makes that account's database the one in use. Every other
// account's database that holds nothing unsent is deleted; one that holds unsent records is cut
// down to those records (its downloads, its synced records and their photos go), is never opened
// for the person signing in, and is reported as `foreign` for them to decide about: sign in as
// that account to send the records, or discard them (`discardForeign`). `deactivate({ purge })` is
// signing out: with purge, the account's database itself is deleted.
//
// Failing closed: when this code can't tell whether a database holds unsent work (it can't be
// opened, counted or cut down), the database is never deleted by itself and never shown to
// another account: it is kept, sealed. And when a tab learns that the account signed in on the
// browser is not the one it holds (`retire`), it lets go of everything it holds in memory and
// shows nothing until it is reloaded.

import { ACCOUNT_CHANGED_EVENT } from '../api'
import {
  forgetLast,
  type LastUser,
  lastUser,
  markerStuck,
  onAccountSignal,
  rememberUser,
  watchAccount,
} from './account'
import { SchemaTooNew } from './idb'
import { revalidate } from './revalidate'
import { type Download, OfflineStore } from './store'
import { type SyncDeps, SyncEngine } from './sync'

const USERS = 'taskto:offline-users'

// Which users have a database on this browser. IndexedDB.databases() would say, but not everywhere,
// so the browser's own storage keeps the list.
function readUsers(): number[] {
  try {
    const list = JSON.parse(window.localStorage.getItem(USERS) ?? '[]') as unknown
    return Array.isArray(list) ? list.filter((n): n is number => Number.isInteger(n)) : []
  } catch {
    return []
  }
}

function writeUsers(list: number[]) {
  try {
    window.localStorage.setItem(USERS, JSON.stringify([...new Set(list)]))
  } catch {
    // Not kept: the database is still claimed by its name.
  }
}

export { type LastUser, lastUser, rememberUser }

export type Runtime = {
  user: number
  store: OfflineStore
  engine: SyncEngine
  stop: () => void
}

// Unsent records of other accounts this browser holds: whose, and how many. They are not shown or
// sent; the person signing in chooses what happens to them.
export type Foreign = { users: number[]; count: number }

export type RuntimeState = {
  runtime: Runtime | null
  // Why there is no runtime: the database is of a newer version, or can't be opened, or the account
  // signed in on this browser is no longer this tab's ('account': nothing is shown until it is
  // reloaded). Never a reason to show another account's.
  problem: 'schema' | 'unavailable' | 'account' | null
  foreign: Foreign | null
}

let state: RuntimeState = { runtime: null, problem: null, foreign: null }
let opening: Promise<RuntimeState> | null = null
const listeners = new Set<() => void>()
let deps: SyncDeps = {}

function set(next: RuntimeState) {
  state = next
  for (const fn of [...listeners]) fn()
}

export const subscribeRuntime = (fn: () => void) => {
  listeners.add(fn)
  return () => listeners.delete(fn)
}
export const runtimeState = () => state

// For tests: the engine's clock, transport and timers.
export function setSyncDeps(next: SyncDeps) {
  deps = next
}

// Every other account's database on this browser goes, except one with unsent work: that one keeps
// only the unsent records (with their photos), which are the person's own and not anyone else's to
// read. What is left is reported, for the person signing in to decide about. A database that can't
// be told apart (it can't be opened, or read, or cut down: a newer version's, or a storage error
// that may pass) is never deleted: it stays as it is, sealed, and is looked at again at the next
// sign-in. Losing someone's unsent records is worse than keeping a database too long.
async function sweep(user: number): Promise<Foreign | null> {
  const others = readUsers().filter((id) => id !== user)
  const keep: number[] = []
  const foreign: Foreign = { users: [], count: 0 }
  for (const id of others) {
    let store: OfflineStore
    try {
      store = await OfflineStore.open(id)
    } catch {
      keep.push(id)
      continue
    }
    try {
      const unsent = await store.unsynced()
      if (unsent.length > 0) {
        keep.push(id)
        foreign.users.push(id)
        foreign.count += unsent.length
        await store.keepOnlyUnsent()
        store.close()
      } else {
        await store.destroy()
      }
    } catch {
      // Not read, cut down or deleted: whatever it holds stays, and is never opened for this person.
      store.close()
      if (!keep.includes(id)) keep.push(id)
    }
  }
  writeUsers([...keep, user])
  return foreign.count > 0 ? foreign : null
}

// This tab learns that the account signed in on this browser is no longer the one it holds (another
// tab signed in as someone else, or out; the server refused a request as another account's): it
// stops sending, closes its connection to the database and drops what it holds, and says only that
// the account changed. It shows nothing of the account's records or photos, and is reloaded to go on.
function retire() {
  const runtime = state.runtime
  if (!runtime) return
  runtime.stop()
  set({ runtime: null, problem: 'account', foreign: null })
  // The connection closes once what the engine was writing (an item put back to waiting, a claim
  // let go) is written: a send in flight is aborted, and finishes at once.
  void runtime.engine
    .idle()
    .catch(() => {})
    .finally(() => runtime.store.close())
}

let watching = false
function watch() {
  if (watching) return
  watching = true
  watchAccount()
  onAccountSignal((who) => {
    if (state.runtime && state.runtime.user !== who) retire()
  })
  window.addEventListener(ACCOUNT_CHANGED_EVENT, (e) => {
    // A request made for the account this tab holds was refused as another's.
    const expected = (e as CustomEvent<{ expected?: number }>).detail?.expected
    if (state.runtime && state.runtime.user === expected) retire()
  })
}

// Makes user's database the one in use, and starts sending their queue. `name` says who signed in
// (online); without it this is an offline start, for the account the browser's marker names.
export async function activate(user: number, name?: string): Promise<RuntimeState> {
  watch()
  if (opening) await opening.catch(() => {})
  if (state.runtime?.user === user) {
    if (name) rememberUser({ id: user, name })
    state.runtime.engine.resume()
    // The same person back after the server couldn't be reached (the app opened offline): what they
    // downloaded is asked about again, since what they may keep may have changed meanwhile.
    void revalidate(state.runtime.store).catch(() => {})
    return state
  }
  const attempt = (async () => {
    await teardown(false)
    // A browser whose marker can't be trusted opens no database offline.
    if (!name && markerStuck()) {
      set({ runtime: null, problem: 'unavailable', foreign: null })
      return state
    }
    // The browser is this account's from here on, whatever becomes of opening its database: an
    // error never leaves the last account's name for the next start to open. When the marker can't
    // be moved it is cleared, and when it can't be, offline starts are refused (account.ts).
    if (name) rememberUser({ id: user, name })
    const foreign = await sweep(user)
    try {
      const store = await OfflineStore.open(user)
      const engine = new SyncEngine(store, {
        // Who this browser says is signed in: a queue is sent only as its owner.
        signedIn: () => lastUser()?.id ?? null,
        // Back online: what was downloaded is asked about before the queue is sent.
        revalidate: () => revalidate(store),
        ...deps,
        // The app can't be opened any more: the copy of its records goes, the queue stays.
        onRevoked: async (item, why) => {
          await store.removeDownload(item)
          await deps.onRevoked?.(item, why)
        },
      })
      const stop = engine.start()
      set({ runtime: { user, store, engine, stop }, problem: null, foreign })
      // Whether what was downloaded may still be kept (access lost, downloads turned off, fewer
      // records readable): asked as soon as the server can be, and a refusal removes the copy.
      void revalidate(store).catch(() => {})
    } catch (e) {
      set({ runtime: null, problem: e instanceof SchemaTooNew ? 'schema' : 'unavailable', foreign })
    }
    return state
  })()
  opening = attempt
  return attempt
}

// The person chose to discard the other accounts' unsent records: everything of theirs on this
// device is deleted.
export async function discardForeign(): Promise<void> {
  if (opening) await opening.catch(() => {})
  const foreign = state.foreign
  if (!foreign) return
  for (const id of foreign.users) await OfflineStore.destroyFor(id).catch(() => {})
  writeUsers(readUsers().filter((id) => !foreign.users.includes(id)))
  set({ ...state, foreign: null })
}

// What signing out would lose: how many records of user's the server hasn't acknowledged, counted
// in the open database if it is the one in use, else by opening the user's (and closing it). A
// database that can't be opened or counted is not zero: `unreadable` says why, and the person is
// told that unsent records may exist that this version can't check.
export type Unsent = { count: number; unreadable: 'schema' | 'unavailable' | null }

export async function unsentOf(user: number): Promise<Unsent> {
  const runtime = state.runtime
  // A browser with no IndexedDB at all can hold nothing: that is zero, not unknown.
  if (typeof indexedDB === 'undefined') return { count: 0, unreadable: null }
  try {
    if (runtime?.user === user) return { count: (await runtime.store.unsynced()).length, unreadable: null }
    const store = await OfflineStore.open(user)
    try {
      return { count: (await store.unsynced()).length, unreadable: null }
    } finally {
      store.close()
    }
  } catch (e) {
    return { count: 0, unreadable: e instanceof SchemaTooNew ? 'schema' : 'unavailable' }
  }
}

async function teardown(purge: boolean, user?: number): Promise<void> {
  const runtime = state.runtime
  if (runtime) {
    runtime.stop()
    if (purge) {
      await runtime.store.destroy()
      writeUsers(readUsers().filter((id) => id !== runtime.user))
      forgetLast(runtime.user)
    } else {
      runtime.store.close()
    }
    set({ runtime: null, problem: null, foreign: state.foreign })
  }
  // Signing out deletes the account's database whether or not it was opened here (the invite
  // page, a database of a newer version, one that could not be opened).
  if (purge && user !== undefined && user !== runtime?.user) {
    await OfflineStore.destroyFor(user).catch(() => {})
    writeUsers(readUsers().filter((id) => id !== user))
    forgetLast(user)
  }
  if (purge && !runtime && user === undefined) forgetLast()
  if (purge && state.problem) set({ runtime: null, problem: null, foreign: state.foreign })
}

// Lets go of the user's database: it is closed, and with `purge` deleted, which is what signing
// out does. `user` is who is signing out: their database goes even if it was never opened here.
// With `keep`, the person chose to sign out without deleting a database whose unsent records this
// version couldn't check: it stays (and stays listed, for its owner's next sign-in to deal with),
// and the browser is nobody's.
export async function deactivate({
  purge,
  user,
  keep,
}: {
  purge: boolean
  user?: number
  keep?: boolean
}): Promise<void> {
  if (opening) await opening.catch(() => {})
  if (keep && !purge) {
    if (user !== undefined) writeUsers([...readUsers(), user])
    await teardown(false)
    forgetLast(user)
    return
  }
  await teardown(purge, user)
}

// Removes every downloaded app of the user's, and what was queued for it stays. (Used when the
// device is told it may not keep them.)
export async function forgetDownloads(store: OfflineStore): Promise<void> {
  for (const d of await store.listDownloads()) await store.removeDownload(d)
}

export type { Download }
