// Which account this browser is signed in as, as far as the offline code can tell, and how a tab
// learns that it changed.
//
// The *last user* marker is enough to open the app offline, where the server can't be asked who
// the session belongs to. (It is not a credential: the server decides every send.) It is also
// what every tab reads to know whether what it holds in memory is still the signed-in account's:
// when another tab signs in as someone else, or out, this tab hears (the `storage` event, a
// BroadcastChannel, or the server's `session_account_changed`) and lets go of the offline view
// (runtime.ts, `retire`), so a stale page never shows the last account's records or photos.
//
// Failing closed: a marker that can't be moved to the new account is cleared; one that can't be
// cleared either is *refused*, and the refusal is kept where a new page finds it (a flag in the
// browser's storage, in the session's storage and in a small database of its own: whichever of
// them can be written), so no page opens the last account's database offline from a marker that
// survived while another account was the last to sign in. A refusal ends when a sign-in moves the
// marker. Where not even a flag can be kept, the browser's storage is read-only altogether, and
// that is the one case this can't see across pages (docs/offline.md).

import { deleteDatabase, openDatabase, runTx } from './idb'

const LAST = 'taskto:offline-last'
const REFUSED = 'taskto:offline-refused'
const FLAGS = 'taskto-offline-flags'
const CHANNEL = 'taskto:offline-account'

export type LastUser = { id: number; name: string }

// The marker could not be moved or cleared: what it names may be the previous account.
let stuck = false

function flagged(storage: () => Storage): boolean {
  try {
    return storage().getItem(REFUSED) !== null
  } catch {
    return false
  }
}

// Whether offline starts are refused here, as far as can be told without waiting.
export const refusedNow = () => stuck || flagged(() => window.localStorage) || flagged(() => window.sessionStorage)

const flagDatabase = () =>
  openDatabase(FLAGS, 1, {
    1: (db) => {
      db.createObjectStore('flags', { keyPath: 'key' })
    },
  })

// The same, asked of every place a refusal is kept (the small database's answer takes a moment).
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

// Keeps (or lifts) the refusal everywhere it can be kept.
function refuse(on: boolean) {
  for (const storage of [() => window.localStorage, () => window.sessionStorage]) {
    try {
      if (on) storage().setItem(REFUSED, '1')
      else storage().removeItem(REFUSED)
    } catch {
      // Another place keeps it.
    }
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

export function lastUser(): LastUser | null {
  if (refusedNow()) return null
  try {
    const v = JSON.parse(window.localStorage.getItem(LAST) ?? 'null') as Partial<LastUser> | null
    return v && Number.isInteger(v.id) && typeof v.name === 'string' ? (v as LastUser) : null
  } catch {
    return null
  }
}

type Listener = (user: number | null) => void
const listeners = new Set<Listener>()

// Hears that the account signed in on this browser is (now) user, or nobody (null).
export function onAccountSignal(fn: Listener): () => void {
  listeners.add(fn)
  return () => listeners.delete(fn)
}

function notify(user: number | null) {
  for (const fn of [...listeners]) fn(user)
}

// The channel the other tabs hear this one on, made when first needed (and again if the browser's
// constructor is another one: a test's).
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
  } catch {
    // The storage event still tells the other tabs.
  }
}

let installed = false
// Starts listening to the other tabs (once): the marker changing in storage, and the channel.
export function watchAccount() {
  if (installed) return
  installed = true
  broadcast()
  window.addEventListener('storage', (e: StorageEvent) => {
    // `key` is null when the whole storage was cleared.
    if (e.key !== null && e.key !== LAST) return
    notify(lastUser()?.id ?? null)
  })
}

export type Remembered = 'ok' | 'cleared' | 'stuck'

// Makes user the browser's. If the marker can't be written it is removed, so an offline start never
// opens the previous account's database; if it can't be removed either, offline starts are refused
// ('stuck'), here and, through the kept refusal, on every page that opens after.
export function rememberUser(user: LastUser): Remembered {
  announce(user.id)
  try {
    window.localStorage.setItem(LAST, JSON.stringify(user))
    stuck = false
    refuse(false)
    return 'ok'
  } catch {
    // Not written: the old one must not stay, unless it already is this user's.
  }
  try {
    const kept = JSON.parse(window.localStorage.getItem(LAST) ?? 'null') as Partial<LastUser> | null
    if (kept?.id === user.id) {
      stuck = false
      refuse(false)
      return 'ok'
    }
  } catch {
    // Unreadable: not this user's, as far as can be told.
  }
  try {
    window.localStorage.removeItem(LAST)
    if (window.localStorage.getItem(LAST) === null) {
      stuck = false
      refuse(false)
      return 'cleared'
    }
  } catch {
    // Not removed either.
  }
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
  } catch {
    // Nothing kept.
  }
}

// The account changed under a page that holds user's data.
export class AccountChanged extends Error {
  constructor() {
    super('the account signed in on this browser changed')
    this.name = 'AccountChanged'
  }
}

// What a response may be written into the database of user's only while the browser is still
// signed in as them: a page that learned of another account's sign-in since it asked (and was
// answered) must not write the answer into a database the new account's sign-in has swept. A
// browser that doesn't say who is signed in is no proof either.
export function assertSignedIn(user: number) {
  const who = lastUser()
  if (who?.id === user) return
  notify(who?.id ?? null)
  throw new AccountChanged()
}
