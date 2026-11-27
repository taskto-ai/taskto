import { deleteDatabase, openDatabase, runTx } from './idb'

const LAST = 'taskto:offline-last'
const REFUSED = 'taskto:offline-refused'
const FLAGS = 'taskto-offline-flags'
const CHANNEL = 'taskto:offline-account'

export type LastUser = { id: number; name: string }

let stuck = false

function flagged(storage: () => Storage): boolean {
  try {
    return storage().getItem(REFUSED) !== null
  } catch {
    return false
  }
}

export const refusedNow = () => stuck || flagged(() => window.localStorage) || flagged(() => window.sessionStorage)

const flagDatabase = () =>
  openDatabase(FLAGS, 1, {
    1: (db) => {
      db.createObjectStore('flags', { keyPath: 'key' })
    },
  })

export async function offlineRefused(): Promise<boolean> {
  await pendingFlag.catch(() => {})
  if (refusedNow()) return true
  try {
    const db = await flagDatabase()
    try {
      return (await runTx(db, ['flags'], 'readonly', (tx) => tx.store('flags').get(REFUSED))) !== undefined
    } finally {
      db.close()
    }
  } catch {
    return false
  }
}

let pendingFlag: Promise<unknown> = Promise.resolve()

function refuse(on: boolean) {
  for (const storage of [() => window.localStorage, () => window.sessionStorage]) {
    try {
      if (on) storage().setItem(REFUSED, '1')
      else storage().removeItem(REFUSED)
    } catch {}
  }
  pendingFlag = pendingFlag
    .catch(() => {})
    .then(async () => {
      if (!on && typeof indexedDB !== 'undefined') {
        await deleteDatabase(FLAGS)
        return
      }
      const db = await flagDatabase()
      try {
        await runTx(db, ['flags'], 'readwrite', (tx) => tx.store('flags').put({ key: REFUSED }))
      } finally {
        db.close()
      }
    })
    .catch(() => {})
}

function markerOf(raw: string | null): LastUser | null {
  if (refusedNow()) return null
  try {
    const v = JSON.parse(raw ?? 'null') as Partial<LastUser> | null
    return v && Number.isInteger(v.id) && typeof v.name === 'string' ? (v as LastUser) : null
  } catch {
    return null
  }
}

export function lastUser(): LastUser | null {
  try {
    return markerOf(window.localStorage.getItem(LAST))
  } catch {
    return null
  }
}

type Listener = (user: number | null) => void
const listeners = new Set<Listener>()

export function onAccountSignal(fn: Listener): () => void {
  listeners.add(fn)
  return () => listeners.delete(fn)
}

function notify(user: number | null) {
  for (const fn of [...listeners]) fn(user)
}

let channel: { ctor: unknown; chan: BroadcastChannel | null } | undefined
function broadcast(): BroadcastChannel | null {
  const ctor = typeof BroadcastChannel === 'undefined' ? undefined : BroadcastChannel
  if (channel && channel.ctor === ctor) return channel.chan
  let chan: BroadcastChannel | null = null
  try {
    chan = ctor ? new ctor(CHANNEL) : null
    chan?.addEventListener('message', (e: MessageEvent) => {
      const who = (e.data as { user?: unknown } | null)?.user
      notify(typeof who === 'number' ? who : null)
    })
  } catch {
    chan = null
  }
  channel = { ctor, chan }
  return chan
}

function announce(user: number | null) {
  try {
    broadcast()?.postMessage({ user })
  } catch {}
}

let installed = false
export function watchAccount() {
  if (installed) return
  installed = true
  broadcast()
  window.addEventListener('storage', (e: StorageEvent) => {
    if (e.key !== null && e.key !== LAST) return
    notify(e.key === null ? null : (markerOf(e.newValue)?.id ?? null))
  })
}

export type Remembered = 'ok' | 'cleared' | 'stuck'

export function rememberUser(user: LastUser): Remembered {
  announce(user.id)
  try {
    window.localStorage.setItem(LAST, JSON.stringify(user))
    stuck = false
    refuse(false)
    return 'ok'
  } catch {}
  try {
    const kept = JSON.parse(window.localStorage.getItem(LAST) ?? 'null') as Partial<LastUser> | null
    if (kept?.id === user.id) {
      stuck = false
      refuse(false)
      return 'ok'
    }
  } catch {}
  try {
    window.localStorage.removeItem(LAST)
    if (window.localStorage.getItem(LAST) === null) {
      stuck = false
      refuse(false)
      return 'cleared'
    }
  } catch {}
  stuck = true
  refuse(true)
  return 'stuck'
}

export function forgetLast(user?: number) {
  try {
    if (user === undefined || lastUser()?.id === user) {
      window.localStorage.removeItem(LAST)
      announce(null)
    }
  } catch {}
}

export class AccountChanged extends Error {
  constructor() {
    super('the account signed in on this browser changed')
    this.name = 'AccountChanged'
  }
}

export function assertSignedIn(user: number) {
  const who = lastUser()
  if (who?.id === user) return
  notify(who?.id ?? null)
  throw new AccountChanged()
}
