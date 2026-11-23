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
  offlineRefused,
  onAccountSignal,
  rememberUser,
  watchAccount,
} from './account'
import { SchemaTooNew } from './idb'
import { revalidate } from './revalidate'
import { type Approval, type Download, OfflineStore, seenOf, type UnsentRef } from './store'
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
async function sweep(user: number, current: () => boolean): Promise<Foreign | null> {
  const others = readUsers().filter((id) => id !== user)
  const keep: number[] = []
  const foreign: Foreign = { users: [], count: 0 }
  for (const id of others) {
    // Whoever signs in here now is the one these databases are dealt with for: if that changed
    // while this ran, nothing more is touched.
    if (!current()) throw new Lapsed()
    let store: OfflineStore
    try {
      store = await OfflineStore.open(id)
    } catch {
      keep.push(id)
      continue
    }
    try {
      // One transaction decides: nothing unsent and the database is marked and deleted, or only
      // the unsent records stay. A record a stale tab commits meanwhile is counted, never deleted.
      const unsent = await store.retireOrKeepUnsent()
      if (unsent.length === 0) {
        await store.destroy()
      } else {
        keep.push(id)
        foreign.users.push(id)
        foreign.count += unsent.length
        store.close()
      }
    } catch {
      // Not read, cut down or deleted: whatever it holds stays, and is never opened for this person.
      store.close()
      if (!keep.includes(id)) keep.push(id)
    }
  }
  if (!current()) throw new Lapsed()
  writeUsers([...keep, user])
  return foreign.count > 0 ? foreign : null
}

// The account signed in on this browser changed while an activation was under way.
class Lapsed extends Error {}

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
    if (!name && (await offlineRefused())) {
      set({ runtime: null, problem: 'unavailable', foreign: null })
      return state
    }
    // The browser is this account's from here on, whatever becomes of opening its database: an
    // error never leaves the last account's name for the next start to open. When the marker can't
    // be moved it is cleared, and when it can't be, offline starts are refused (account.ts).
    const marker = name ? rememberUser({ id: user, name }) : 'ok'
    // The account epoch: who the browser's marker names. Every step of this that waits re-checks
    // it before it deletes, opens or shows anything, so an account that signed in meanwhile (in
    // another tab: this tab has no runtime yet to hear it with) is never acted against or shown
    // this account's database.
    const current = () => {
      const who = lastUser()
      return marker === 'ok' ? who?.id === user : who === null || who.id === user
    }
    const abandon = (store?: OfflineStore) => {
      store?.close()
      set({ runtime: null, problem: 'account', foreign: null })
      return state
    }
    if (!current()) return abandon()
    let foreign: Foreign | null
    try {
      foreign = await sweep(user, current)
    } catch (e) {
      if (e instanceof Lapsed) return abandon()
      throw e
    }
    let store: OfflineStore | undefined
    try {
      store = await OfflineStore.open(user)
      if (!current()) return abandon(store)
      const opened = store
      const engine = new SyncEngine(opened, {
        // Who this browser says is signed in: a queue is sent only as its owner.
        signedIn: () => lastUser()?.id ?? null,
        // Back online: what was downloaded is asked about before the queue is sent.
        revalidate: () => revalidate(opened),
        ...deps,
        // The app can't be opened any more: the copy of its records goes, the queue stays.
        onRevoked: async (item, why) => {
          await opened.removeDownload(item)
          await deps.onRevoked?.(item, why)
        },
      })
      const stop = engine.start()
      set({ runtime: { user, store: opened, engine, stop }, problem: null, foreign })
      // Whether what was downloaded may still be kept (access lost, downloads turned off, fewer
      // records readable): asked as soon as the server can be, and a refusal removes the copy.
      void revalidate(opened).catch(() => {})
    } catch (e) {
      store?.close()
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
// `items`: each unsent record as it is now (its id, version and operation): what the person is
// shown, and what they approve to lose (`approvalOf`): an item that changed since is not that.
export type Unsent = { count: number; items: UnsentRef[]; unreadable: 'schema' | 'unavailable' | null }

export async function unsentOf(user: number): Promise<Unsent> {
  const runtime = state.runtime
  const found = (items: { id: string; version?: number; operation: string }[]): Unsent => ({
    count: items.length,
    items: items.map((i) => ({ id: i.id, ...seenOf(i) })),
    unreadable: null,
  })
  // A browser with no IndexedDB at all can hold nothing: that is zero, not unknown.
  if (typeof indexedDB === 'undefined') return found([])
  try {
    if (runtime?.user === user) return found(await runtime.store.unsynced())
    const store = await OfflineStore.open(user)
    try {
      return found(await store.unsynced())
    } finally {
      store.close()
    }
  } catch (e) {
    return { count: 0, items: [], unreadable: e instanceof SchemaTooNew ? 'schema' : 'unavailable' }
  }
}

// The first half of signing out, and the only thing that decides whether a database may go: in one
// transaction per database (the user's, and the one this tab holds), it is checked that nothing
// unsent is in it beyond `approved` (what the person was shown and agreed to lose), and if so the
// database is emptied and marked as being deleted, so nothing can be committed to it from then on
// (a stale tab, another tab of the same account). An approval is of an item as it was shown (its
// version and its operation), so a record that was changed since, or made after, is not agreed to.
// Answers the unsent records that are there and were not approved, as they are now: then nothing
// was changed, and the person is asked again. A database that can't be opened is not looked at
// here: the person already chose what happens to it.
export async function settle(user: number | undefined, approved: Approval = new Map()): Promise<UnsentRef[]> {
  if (opening) await opening.catch(() => {})
  const runtime = state.runtime
  const users = [...new Set([runtime?.user, user].filter((n): n is number => n !== undefined))]
  const left: UnsentRef[] = []
  for (const id of users) {
    let store = runtime?.user === id ? runtime.store : undefined
    const own = !store
    try {
      store ??= await OfflineStore.open(id)
    } catch {
      continue
    }
    try {
      left.push(...(await store.retireOrKeepUnsent(approved, false)))
    } catch {
      // Not read: the person's choice (they were told) decides, as it did.
    } finally {
      if (own) store.close()
    }
  }
  return left
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
  approved,
}: {
  purge: boolean
  user?: number
  keep?: boolean
  approved?: Approval
}): Promise<UnsentRef[]> {
  if (opening) await opening.catch(() => {})
  if (keep && !purge) {
    if (user !== undefined) writeUsers([...readUsers(), user])
    await teardown(false)
    forgetLast(user)
    return []
  }
  // Deleting is conditional on there being nothing unsent that the person didn't approve: if some
  // appeared since they were asked, everything stays (and is the answer, for the caller to say).
  if (purge) {
    const left = await settle(user, approved)
    if (left.length > 0) return left
  }
  await teardown(purge, user)
  return []
}

// Removes every downloaded app of the user's, and what was queued for it stays. (Used when the
// device is told it may not keep them.)
export async function forgetDownloads(store: OfflineStore): Promise<void> {
  for (const d of await store.listDownloads()) await store.removeDownload(d)
}

export type { Download }
