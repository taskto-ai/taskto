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

import { deleteDatabase, SchemaTooNew } from './idb'
import { revalidate } from './revalidate'
import { type Download, OfflineStore } from './store'
import { type SyncDeps, SyncEngine } from './sync'

const USERS = 'taskto:offline-users'
const LAST = 'taskto:offline-last'

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

// Who signed in here last: enough to open the app offline, where the server can't be asked who
// the session belongs to. (Not a credential: the server decides every send.)
export type LastUser = { id: number; name: string }

export function rememberUser(user: LastUser) {
  try {
    window.localStorage.setItem(LAST, JSON.stringify(user))
  } catch {
    // No storage: the app can't open offline, which it couldn't without it anyway.
  }
}

export function lastUser(): LastUser | null {
  try {
    const v = JSON.parse(window.localStorage.getItem(LAST) ?? 'null') as Partial<LastUser> | null
    return v && Number.isInteger(v.id) && typeof v.name === 'string' ? (v as LastUser) : null
  } catch {
    return null
  }
}

function forgetLast(user?: number) {
  try {
    if (user === undefined || lastUser()?.id === user) window.localStorage.removeItem(LAST)
  } catch {
    // Nothing kept.
  }
}

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
  // Why there is no runtime: the database is of a newer version, or can't be opened. Never a reason
  // to show another account's.
  problem: 'schema' | 'unavailable' | null
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
// read. What is left is reported, for the person signing in to decide about.
async function sweep(user: number): Promise<Foreign | null> {
  const others = readUsers().filter((id) => id !== user)
  const keep: number[] = []
  const foreign: Foreign = { users: [], count: 0 }
  for (const id of others) {
    try {
      const store = await OfflineStore.open(id)
      const unsent = await store.unsynced()
      if (unsent.length > 0) {
        await store.keepOnlyUnsent()
        store.close()
        keep.push(id)
        foreign.users.push(id)
        foreign.count += unsent.length
      } else {
        await store.destroy()
      }
    } catch (e) {
      // One that can't be read (a newer version's) stays as it is, for its owner's page to open; any
      // other is deleted.
      if (e instanceof SchemaTooNew) keep.push(id)
      else await deleteDatabase(OfflineStore.databaseName(id)).catch(() => {})
    }
  }
  writeUsers([...keep, user])
  return foreign.count > 0 ? foreign : null
}

// Makes user's database the one in use, and starts sending their queue.
export async function activate(user: number, name?: string): Promise<RuntimeState> {
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
    // The browser is this account's from here on, whatever becomes of opening its database: an
    // error never leaves the last account's name for the next start to open.
    if (name) rememberUser({ id: user, name })
    const foreign = await sweep(user)
    try {
      const store = await OfflineStore.open(user)
      const engine = new SyncEngine(store, {
        // Who this browser says is signed in: a queue is sent only as its owner.
        signedIn: () => lastUser()?.id ?? null,
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

// How many records of user's the server hasn't acknowledged: what signing out would lose. Counted
// in the open database if it is the one in use, else by opening the user's (and closing it).
export async function unsentOf(user: number): Promise<number> {
  const runtime = state.runtime
  if (runtime?.user === user) return (await runtime.store.unsynced()).length
  try {
    const store = await OfflineStore.open(user)
    try {
      return (await store.unsynced()).length
    } finally {
      store.close()
    }
  } catch {
    return 0
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
export async function deactivate({ purge, user }: { purge: boolean; user?: number }): Promise<void> {
  if (opening) await opening.catch(() => {})
  await teardown(purge, user)
}

// Removes every downloaded app of the user's, and what was queued for it stays. (Used when the
// device is told it may not keep them.)
export async function forgetDownloads(store: OfflineStore): Promise<void> {
  for (const d of await store.listDownloads()) await store.removeDownload(d)
}

export type { Download }
