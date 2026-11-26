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
//
// A logout that got no answer (`unanswered`) may still reach the server later, whatever Back saw.
// Back then keeps the record, as `backed`: the person is in the app again (it doesn't hold the
// account closed or its queue back, and no start takes it up), and if that login ends while the
// record is there (a 401, a start with nobody signed in) the sign-out is finished as they decided.

import type { Approval, Seen } from './store'

const KEY = 'taskto:sign-out-pending'

export type SignOutPhase = 'requested' | 'confirmed' | 'backed'

export type PendingSignOut = {
  user: number
  // The login being signed out: '' when the server didn't name it.
  session: string
  phase: SignOutPhase
  keep: boolean
  approved: [string, Seen][]
  // A logout of it got no answer: it may yet end the login.
  unanswered?: boolean
  // The person was told the database couldn't be read, and chose to discard it.
  discardUnreadable?: boolean
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
    (r.phase === 'requested' || r.phase === 'confirmed' || r.phase === 'backed') &&
    typeof r.keep === 'boolean' &&
    (r.unanswered === undefined || typeof r.unanswered === 'boolean') &&
    (r.discardUnreadable === undefined || typeof r.discardUnreadable === 'boolean') &&
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
export function recordSignOut(
  user: number,
  session: string,
  keep: boolean,
  approved: Approval,
  discardUnreadable = false,
): void {
  write([
    ...read().filter((p) => p.user !== user),
    {
      user,
      session,
      phase: 'requested',
      keep,
      approved: [...approved],
      ...(discardUnreadable ? { discardUnreadable } : {}),
    },
  ])
}

// A logout of it got no answer: it may reach the server yet.
export function unansweredSignOut(user: number): void {
  write(read().map((p) => (p.user === user ? { ...p, unanswered: true } : p)))
}

// Back: the same login is this browser's still. The sign-out is over, unless a logout of it got no
// answer, which may still end the login: then it is kept, as `backed`, to be finished if it does.
export function backFromSignOut(user: number): void {
  const p = read().find((r) => r.user === user)
  if (p?.unanswered) write(read().map((r) => (r.user === user ? { ...r, phase: 'backed' } : r)))
  else settleSignOut(user)
}

// The sign-out the person went back from, of the login `session`, if its logout may still end it.
export function backedSignOut(user: number, session: string | undefined): PendingSignOut | undefined {
  return read().find((p) => p.user === user && p.phase === 'backed' && !!session && p.session === session)
}

// The server confirmed that `user`'s login `session` is over: what is left of its sign-out is the
// device's cleanup. A record of another login of the account (a newer one's, which the person went
// back from) stays as it is.
export function confirmSignOut(user: number, session: string): void {
  write(read().map((p) => (p.user === user && p.session === session ? { ...p, phase: 'confirmed' } : p)))
}

// The sign-out is over: finished and verified, or cancelled by the same login.
export function settleSignOut(user: number): void {
  write(read().filter((p) => p.user !== user))
}

// Whether `user`'s data on this device belongs to a sign-out that isn't resolved: not to be opened
// offline, and not to be sent from. One the person went back from (`backed`) is not: they are in the
// app again.
export function signOutPending(user: number): boolean {
  return read().some((p) => p.user === user && p.phase !== 'backed')
}

// The login this browser started with last (a sign-in, or a start that found one), shared by its
// tabs: a sign-out of an older login of the same account, confirmed late, leaves what is on the
// device to it (signOut.ts).
const LOGIN = 'taskto:login'

export function rememberLogin(user: number, session: string | undefined): void {
  try {
    if (session) window.localStorage.setItem(LOGIN, JSON.stringify({ user, session }))
    else window.localStorage.removeItem(LOGIN)
  } catch {
    // Not kept: a late sign-out of an older login clears the device as it would have.
  }
}

export function browserLogin(): { user: number; session: string } | null {
  try {
    const v = JSON.parse(window.localStorage.getItem(LOGIN) ?? 'null') as { user?: unknown; session?: unknown } | null
    return v && Number.isInteger(v.user) && typeof v.session === 'string' && v.session !== ''
      ? { user: v.user as number, session: v.session }
      : null
  } catch {
    return null
  }
}
