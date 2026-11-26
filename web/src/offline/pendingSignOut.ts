// A sign-out that has become certain and isn't finished yet, kept in the browser before anything of
// it waits: who is signing out, and what they decided (to keep the database, or which unsent records
// they agreed to lose, as they were shown). It outlives the tab: if the tab closes while the server
// is asked or the device is cleared, the next start finishes it with the same decision, and until
// then no offline start opens that account's data. It goes only once the device was verified clear.

import type { Approval, Seen } from './store'

const KEY = 'taskto:sign-out-pending'

export type PendingSignOut = { user: number; keep: boolean; approved: [string, Seen][] }

function read(): PendingSignOut[] {
  try {
    const v = JSON.parse(window.localStorage.getItem(KEY) ?? '[]') as unknown
    if (!Array.isArray(v)) return []
    return v.filter(
      (p): p is PendingSignOut =>
        !!p && Number.isInteger((p as PendingSignOut).user) && Array.isArray((p as PendingSignOut).approved),
    )
  } catch {
    return []
  }
}

function write(list: PendingSignOut[]) {
  try {
    if (list.length === 0) window.localStorage.removeItem(KEY)
    else window.localStorage.setItem(KEY, JSON.stringify(list))
  } catch {
    // Not kept: the sign-out still goes on in this tab.
  }
}

export function pendingSignOuts(): PendingSignOut[] {
  return read()
}

// Recorded when the sign-out becomes certain, before it asks the server or touches the device.
export function recordSignOut(user: number, keep: boolean, approved: Approval): void {
  write([...read().filter((p) => p.user !== user), { user, keep, approved: [...approved] }])
}

// The sign-out is over: finished and verified, or not to be (the person went back, signed in).
export function settleSignOut(user: number): void {
  write(read().filter((p) => p.user !== user))
}

// Whether `user`'s data on this device belongs to a sign-out that isn't finished: not to be opened.
export function signOutPending(user: number): boolean {
  return read().some((p) => p.user === user)
}
