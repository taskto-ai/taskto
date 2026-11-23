// The offline machinery of the signed-in user: their database and the engine that sends their
// queue, claimed when someone signs in and let go when they sign out.
//
// Like the drafts (uiState.ts), what is kept for offline use belongs to one account at a time.
// `activate(user)` makes that account's database the one in use. Any other account's that has
// nothing waiting for the server is deleted at once; one that has unsent work is left alone, never
// opened by anyone but its owner, and its downloaded records are removed so only the person's own
// unsent input stays. `deactivate({ purge })` is signing out: with purge, the database itself is deleted.

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

export type RuntimeState = { runtime: Runtime | null; problem: 'schema' | 'unavailable' | null }

let state: RuntimeState = { runtime: null, problem: null }
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

// Every other account's database on this browser goes, except one with unsent work, which keeps
// only that work.
async function sweep(user: number) {
  const others = readUsers().filter((id) => id !== user)
  const keep: number[] = []
  for (const id of others) {
    try {
      const store = await OfflineStore.open(id)
      const unsent = await store.unsynced()
      if (unsent.length > 0) {
        for (const d of await store.listDownloads()) await store.removeDownload(d)
        store.close()
        keep.push(id)
      } else {
        await store.destroy()
      }
    } catch (e) {
      // One that can't be read (a newer version's) stays as it is; any other is deleted.
      if (e instanceof SchemaTooNew) keep.push(id)
      else await deleteDatabase(OfflineStore.databaseName(id)).catch(() => {})
    }
  }
  writeUsers([...keep, user])
}

// Makes user's database the one in use, and starts sending their queue.
export async function activate(user: number, name?: string): Promise<RuntimeState> {
  if (opening) await opening.catch(() => {})
  if (state.runtime?.user === user) {
    if (name) rememberUser({ id: user, name })
    state.runtime.engine.resume()
    return state
  }
  const attempt = (async () => {
    await teardown(false)
    await sweep(user)
    try {
      const store = await OfflineStore.open(user)
      const engine = new SyncEngine(store, {
        ...deps,
        // The app can't be opened any more: the copy of its records goes, the queue stays.
        onRevoked: async (item, why) => {
          await store.removeDownload(item)
          await deps.onRevoked?.(item, why)
        },
      })
      const stop = engine.start()
      if (name) rememberUser({ id: user, name })
      set({ runtime: { user, store, engine, stop }, problem: null })
      // Whether what was downloaded may still be kept (access lost, downloads turned off): asked
      // as soon as the server can be, and a refusal removes the copy.
      void revalidate(store).catch(() => {})
    } catch (e) {
      set({ runtime: null, problem: e instanceof SchemaTooNew ? 'schema' : 'unavailable' })
    }
    return state
  })()
  opening = attempt
  return attempt
}

async function teardown(purge: boolean): Promise<void> {
  const runtime = state.runtime
  if (!runtime) {
    if (purge) forgetLast()
    return
  }
  runtime.stop()
  if (purge) {
    await runtime.store.destroy()
    writeUsers(readUsers().filter((id) => id !== runtime.user))
    forgetLast(runtime.user)
  } else {
    runtime.store.close()
  }
  set({ runtime: null, problem: null })
}

// Lets go of the user's database: it is closed, and with `purge` deleted, which is what signing
// out does.
export async function deactivate({ purge }: { purge: boolean }): Promise<void> {
  if (opening) await opening.catch(() => {})
  await teardown(purge)
}

// Removes every downloaded app of the user's, and what was queued for it stays. (Used when the
// device is told it may not keep them.)
export async function forgetDownloads(store: OfflineStore): Promise<void> {
  for (const d of await store.listDownloads()) await store.removeDownload(d)
}

export type { Download }
