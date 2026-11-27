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
import { signOutPending } from './pendingSignOut'
import { revalidate } from './revalidate'
import { type Approval, type Download, OfflineStore, seenOf, type UnsentRef } from './store'
import { type SyncDeps, SyncEngine } from './sync'
import { OfflineWriter, type WriterDeps } from './writer'

const USERS = 'taskto:offline-users'

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
  } catch {}
}

export { type LastUser, lastUser, rememberUser }

export type Runtime = {
  user: number
  store: OfflineStore
  engine: SyncEngine
  writer: OfflineWriter
  stop: () => void
}

export type Foreign = { users: number[]; count: number }

export type RuntimeState = {
  runtime: Runtime | null
  problem: 'schema' | 'unavailable' | 'account' | null
  foreign: Foreign | null
}

let state: RuntimeState = { runtime: null, problem: null, foreign: null }
let opening: Promise<RuntimeState> | null = null
const listeners = new Set<() => void>()
let deps: SyncDeps = {}
let writerDeps: WriterDeps = {}

function set(next: RuntimeState) {
  state = next
  for (const fn of [...listeners]) fn()
}

export const subscribeRuntime = (fn: () => void) => {
  listeners.add(fn)
  return () => listeners.delete(fn)
}
export const runtimeState = () => state

export function setSyncDeps(next: SyncDeps, writer: WriterDeps = {}) {
  deps = next
  writerDeps = writer
}

async function sweep(user: number, current: () => boolean): Promise<Foreign | null> {
  const others = readUsers().filter((id) => id !== user)
  const keep: number[] = []
  const foreign: Foreign = { users: [], count: 0 }
  for (const id of others) {
    if (!current()) throw new Lapsed()
    let store: OfflineStore
    try {
      store = await OfflineStore.open(id)
    } catch {
      keep.push(id)
      continue
    }
    try {
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
      store.close()
      if (!keep.includes(id)) keep.push(id)
    }
  }
  if (!current()) throw new Lapsed()
  writeUsers([...keep, user])
  return foreign.count > 0 ? foreign : null
}

class Lapsed extends Error {}

function retire() {
  const runtime = state.runtime
  if (!runtime) return
  runtime.stop()
  set({ runtime: null, problem: 'account', foreign: null })
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
    const expected = (e as CustomEvent<{ expected?: number }>).detail?.expected
    if (state.runtime && state.runtime.user === expected) retire()
  })
}

export async function activate(user: number, name?: string): Promise<RuntimeState> {
  watch()
  if (opening) await opening.catch(() => {})
  if (state.runtime?.user === user) {
    if (name) rememberUser({ id: user, name })
    state.runtime.engine.resume()
    void revalidate(state.runtime.store).catch(() => {})
    return state
  }
  const attempt = (async () => {
    await teardown(false)
    if (!name && ((await offlineRefused()) || signOutPending(user))) {
      set({ runtime: null, problem: 'unavailable', foreign: null })
      return state
    }
    const marker = name ? rememberUser({ id: user, name }) : 'ok'
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
      opened.connectTabs(
        writerDeps.channel ?? ((name) => (typeof BroadcastChannel === 'undefined' ? null : new BroadcastChannel(name))),
      )
      const engine = new SyncEngine(opened, {
        signedIn: () => lastUser()?.id ?? null,
        revalidate: () => revalidate(opened),
        ...deps,
        onRevoked: async (item, why) => {
          await opened.removeDownload(item)
          await deps.onRevoked?.(item, why)
        },
      })
      const writer = new OfflineWriter(opened, writerDeps)
      await writer.start()
      if (!current()) {
        writer.stop()
        return abandon(opened)
      }
      const stopEngine = engine.start()
      const stop = () => {
        stopEngine()
        writer.stop()
      }
      set({ runtime: { user, store: opened, engine, writer, stop }, problem: null, foreign })
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

export async function discardForeign(): Promise<void> {
  if (opening) await opening.catch(() => {})
  const foreign = state.foreign
  if (!foreign) return
  for (const id of foreign.users) await OfflineStore.destroyFor(id).catch(() => {})
  writeUsers(readUsers().filter((id) => !foreign.users.includes(id)))
  set({ ...state, foreign: null })
}

export type Unsent = { count: number; items: UnsentRef[]; unreadable: 'schema' | 'unavailable' | null }

export async function unsentOf(user: number): Promise<Unsent> {
  const runtime = state.runtime
  const found = (items: { id: string; version?: number; operation: string }[]): Unsent => ({
    count: items.length,
    items: items.map((i) => ({ id: i.id, ...seenOf(i) })),
    unreadable: null,
  })
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

export async function settle(user: number | undefined, approved: Approval = new Map()): Promise<UnsentRef[]> {
  return checkUnsent(user, (store) => store.retireOrKeepUnsent(approved, false))
}

export async function stillAgreed(user: number, approved: Approval = new Map()): Promise<UnsentRef[]> {
  return checkUnsent(user, (store) => store.unapproved(approved))
}

async function checkUnsent(
  user: number | undefined,
  ask: (store: OfflineStore) => Promise<UnsentRef[]>,
): Promise<UnsentRef[]> {
  if (opening) await opening.catch(() => {})
  const runtime = state.runtime
  const users = user !== undefined ? [user] : runtime ? [runtime.user] : []
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
      left.push(...(await ask(store)))
    } catch {
    } finally {
      if (own) store.close()
    }
  }
  return left
}

async function teardown(purge: boolean, user?: number): Promise<void> {
  const runtime = user === undefined || state.runtime?.user === user ? state.runtime : null
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
  if (purge && user !== undefined && user !== runtime?.user) {
    await OfflineStore.destroyFor(user).catch(() => {})
    writeUsers(readUsers().filter((id) => id !== user))
    forgetLast(user)
  }
  if (purge && !runtime && user === undefined) forgetLast()
  if (purge && state.problem) set({ runtime: null, problem: null, foreign: state.foreign })
}

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
    await teardown(false, user)
    forgetLast(user)
    return []
  }
  if (purge) {
    const left = await settle(user, approved)
    if (left.length > 0) return left
  }
  await teardown(purge, user)
  return []
}

export async function purgeAccount(user: number, approved: Approval = new Map()): Promise<UnsentRef[]> {
  if (opening) await opening.catch(() => {})
  const left = await settle(user, approved)
  if (left.length > 0) return left
  const runtime = state.runtime
  if (runtime?.user === user) {
    await teardown(true, user)
  } else {
    await OfflineStore.destroyFor(user).catch(() => {})
    writeUsers(readUsers().filter((id) => id !== user))
    forgetLast(user)
  }
  return []
}

export async function forgetDownloads(store: OfflineStore): Promise<void> {
  for (const d of await store.listDownloads()) await store.removeDownload(d)
}

export type { Download }
