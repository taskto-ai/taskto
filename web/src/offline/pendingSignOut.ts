// A sign-out that has become certain and isn't finished yet, kept in the browser (shared by its tabs)
// before anything of it waits: who is signing out, from which login (`session`, as /api/me names
// it), how far it got (`phase`: the server asked, or the server confirmed), and what they decided
// (to keep the database, or which unsent records they agreed to lose, as they were shown).
//
// It outlives the tab. Until it is resolved, no offline start opens the account's data, and no tab's
// queue sends for it. It is resolved by that same login cancelling it (Back, once /api/me says the
// login is still this browser's), or by the login ending (the server confirmed it, or this browser
// no longer holds it) and the device being verified clear; never by another tab merely starting with
// that login, which takes the sign-out up again instead.

import type { Approval, Seen } from './store'

const KEY = 'taskto:sign-out-pending'

export type SignOutPhase = 'requested' | 'confirmed'

export type PendingSignOut = {
  user: number
  // The login being signed out: '' when the server didn't name it.
  session: string
  phase: SignOutPhase
  keep: boolean
  approved: [string, Seen][]
}

const isSeen = (v: unknown): v is Seen =>
  !!v && typeof v === 'object' && Number.isInteger((v as Seen).version) && typeof (v as Seen).operation === 'string'

// The whole record is checked: a record that isn't one (written by something else, cut short) is not
// acted on.
function valid(p: unknown): p is PendingSignOut {
  if (!p || typeof p !== 'object') return false
  const r = p as PendingSignOut
  return (
    Number.isInteger(r.user) &&
    r.user > 0 &&
    typeof r.session === 'string' &&
    (r.phase === 'requested' || r.phase === 'confirmed') &&
    typeof r.keep === 'boolean' &&
    Array.isArray(r.approved) &&
    r.approved.every((a) => Array.isArray(a) && a.length === 2 && typeof a[0] === 'string' && isSeen(a[1]))
  )
}

function read(): PendingSignOut[] {
  let raw: unknown
  try {
    raw = JSON.parse(window.localStorage.getItem(KEY) ?? '[]')
  } catch {
    return []
  }
  return Array.isArray(raw) ? raw.filter(valid) : []
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

export function pendingSignOutOf(user: number): PendingSignOut | undefined {
  return read().find((p) => p.user === user)
}

// Recorded when the sign-out becomes certain, before it asks the server or touches the device.
export function recordSignOut(user: number, session: string, keep: boolean, approved: Approval): void {
  write([
    ...read().filter((p) => p.user !== user),
    { user, session, phase: 'requested', keep, approved: [...approved] },
  ])
}

// The server confirmed it: the login is over, what is left is the device's cleanup.
export function confirmSignOut(user: number): void {
  write(read().map((p) => (p.user === user ? { ...p, phase: 'confirmed' } : p)))
}

// The sign-out is over: finished and verified, or cancelled by the same login.
export function settleSignOut(user: number): void {
  write(read().filter((p) => p.user !== user))
}

// Whether `user`'s data on this device belongs to a sign-out that isn't resolved: not to be opened
// offline, and not to be sent from.
export function signOutPending(user: number): boolean {
  return read().some((p) => p.user === user)
}
