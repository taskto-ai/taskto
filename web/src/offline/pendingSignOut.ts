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
  // When it was recorded (this browser's clock), to tell it from what a start learned before it.
  at?: number
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
    (r.at === undefined || Number.isFinite(r.at)) &&
    Array.isArray(r.approved) &&
    r.approved.every((a) => Array.isArray(a) && a.length === 2 && typeof a[0] === 'string' && isSeen(a[1]))
  )
}

// Each login's record is a key of its own (`KEY:<user>:<session>`), so a tab changing one never
// writes back another tab's from what it read before (two tabs at once, which nothing orders).
// Records an earlier version kept together under `KEY`, as a list, are still read, and move to their
// own key when they change.
const keyOf = (user: number, session: string) => `${KEY}:${user}:${session}`

function legacy(): PendingSignOut[] {
  let raw: unknown
  try {
    raw = JSON.parse(window.localStorage.getItem(KEY) ?? '[]')
  } catch {
    return []
  }
  return Array.isArray(raw) ? raw.filter(valid) : []
}

function read(): PendingSignOut[] {
  const own: PendingSignOut[] = []
  try {
    for (let i = 0; i < window.localStorage.length; i++) {
      const key = window.localStorage.key(i)
      if (!key?.startsWith(`${KEY}:`)) continue
      try {
        const p = JSON.parse(window.localStorage.getItem(key) ?? 'null') as unknown
        if (valid(p) && key === keyOf(p.user, p.session)) own.push(p)
      } catch {
        // Not a record: not acted on.
      }
    }
  } catch {
    return legacy()
  }
  const has = (p: PendingSignOut) => own.some((o) => o.user === p.user && o.session === p.session)
  return [...legacy().filter((p) => !has(p)), ...own]
}

// The record of one login: written (`p`), or removed (null).
// Says whether the record was written (or removed) as asked.
function put(user: number, session: string, p: PendingSignOut | null): boolean {
  try {
    if (p) window.localStorage.setItem(keyOf(user, session), JSON.stringify(p))
    else window.localStorage.removeItem(keyOf(user, session))
  } catch {
    // Not kept: the sign-out still goes on in this tab.
    return false
  }
  try {
    const old = legacy()
    const rest = old.filter((r) => !(r.user === user && r.session === session))
    if (rest.length !== old.length) {
      if (rest.length === 0) window.localStorage.removeItem(KEY)
      else window.localStorage.setItem(KEY, JSON.stringify(rest))
    }
  } catch {
    // The earlier version's list is left as it is; the record's own key is what is read first.
  }
  return true
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
  // One record per login, never per account: a stale tab signing out an older login of the account
  // never takes away the decision of a newer one (recorded in another tab, perhaps gone back from and
  // still waiting on a logout that got no answer). A logout of this same login that got no answer
  // before may still reach the server: that stays known, whatever becomes of this one.
  const before = read().find((p) => p.user === user && p.session === session)
  const unanswered = !!before?.unanswered && session !== ''
  put(user, session, {
    user,
    session,
    phase: 'requested',
    keep,
    approved: [...approved],
    ...(discardUnreadable ? { discardUnreadable } : {}),
    ...(unanswered ? { unanswered } : {}),
    at: Date.now(),
  })
}

// Changes the record of one login, when there is one.
// 'none' when there is no record of that login, else whether the change was written.
function change(
  user: number,
  session: string,
  to: (p: PendingSignOut) => PendingSignOut | null,
): 'none' | 'written' | 'failed' {
  const p = read().find((r) => r.user === user && r.session === session)
  if (!p) return 'none'
  return put(user, session, to(p)) ? 'written' : 'failed'
}

// The logouts of a login on their way, in any tab: each one's own key (so no tab writes back
// another's), written before it is sent and removed once its answer is in (a definite one, or none
// in time, which first marks the record `unanswered`). A tab that closes with one on its way leaves
// its key: that logout may still reach the server, so the sign-out is never dropped for it.
const LOGOUT = 'taskto:sign-out-logout:'
const logoutsOf = (user: number, session: string) => `${LOGOUT}${user}:${session}:`

export function beginLogout(user: number, session: string): string {
  const key = `${logoutsOf(user, session)}${Date.now().toString(36)}${Math.random().toString(36).slice(2, 8)}`
  try {
    window.localStorage.setItem(key, String(Date.now()))
  } catch {
    // Not kept: a Back in another tab meanwhile may drop the sign-out, as before there was this.
  }
  return key
}

// `answered`: the server's own answer came (it is over); without one, it may still reach the server.
// The mark goes only once what it stands for is kept otherwise: a definite answer, or the sign-out
// marked as unanswered. When the browser refuses that write, the mark stays (the safe side).
export function endLogout(key: string, user: number, session: string, answered: boolean): void {
  if (!answered && !unansweredSignOut(user, session)) return
  try {
    window.localStorage.removeItem(key)
  } catch {
    // Left: the sign-out is kept for it, which is the safe side.
  }
}

function logoutsOnTheirWay(user: number, session: string): string[] {
  const keys: string[] = []
  try {
    const prefix = logoutsOf(user, session)
    for (let i = 0; i < window.localStorage.length; i++) {
      const key = window.localStorage.key(i)
      if (key?.startsWith(prefix)) keys.push(key)
    }
  } catch {
    // Not read: none known.
  }
  return keys
}

// A logout of `user`'s login `session` got no answer: it may reach the server yet. A record of
// another login of the account (a newer one's, from another tab) stays as it is.
// False when the browser refused to write it (there is a record, and it isn't marked).
export function unansweredSignOut(user: number, session: string): boolean {
  return change(user, session, (p) => ({ ...p, unanswered: true })) !== 'failed'
}

// Back: `user`'s login `session` is this browser's still. Its sign-out is over, unless a logout of
// it got no answer, which may still end the login: then it is kept, as `backed`, to be finished if
// it does. Only that login's record: another login's of the account (signing out in another tab)
// stays.
// A logout of it on its way in another tab (not answered yet) counts as one that may end the login.
export function backFromSignOut(user: number, session: string): void {
  change(user, session, (p) =>
    p.unanswered || logoutsOnTheirWay(user, session).length > 0 ? { ...p, phase: 'backed' } : null,
  )
}

// The sign-out the person went back from, of the login `session`, if its logout may still end it.
export function backedSignOut(user: number, session: string | undefined): PendingSignOut | undefined {
  return read().find((p) => p.user === user && p.phase === 'backed' && !!session && p.session === session)
}

// The server confirmed that `user`'s login `session` is over: what is left of its sign-out is the
// device's cleanup. A record of another login of the account (a newer one's, which the person went
// back from) stays as it is.
export function confirmSignOut(user: number, session: string): void {
  change(user, session, (p) => ({ ...p, phase: 'confirmed' }))
}

// The sign-out is over: finished and verified, or cancelled by the same login. With `session`, only
// that login's: the sign-out of another login of the account (recorded meanwhile) stays.
export function settleSignOut(user: number, session?: string): void {
  for (const p of read()) {
    if (p.user !== user || (session !== undefined && p.session !== session)) continue
    put(p.user, p.session, null)
    // Its logouts' marks go with it: the sign-out is over.
    for (const key of logoutsOnTheirWay(p.user, p.session)) {
      try {
        window.localStorage.removeItem(key)
      } catch {
        // Left: harmless once the record is gone.
      }
    }
  }
}

// Whether `user`'s data on this device belongs to a sign-out that isn't resolved: not to be opened
// offline, and not to be sent from. One the person went back from (`backed`) is not: they are in the
// app again.
export function signOutPending(user: number): boolean {
  return read().some((p) => p.user === user && p.phase !== 'backed')
}

// What this browser last learned of who is signed in on it, shared by its tabs (one cookie for the
// browser): a login (a sign-in, or a start that found one), or that one ended (`session` ''), with
// when it was learned (`at`, this browser's clock: when the server was asked, or the login seen
// over). A sign-out of an older login of the same account, confirmed late, leaves what is on the
// device to the newer one (signOut.ts). An answer that is older than what is known (a start answered
// late, for this account or another, or for a login that has ended since) is no news: it is not
// remembered, and its caller claims nothing for it.
const LOGIN = 'taskto:login'

export type BrowserLogin = { user: number; session: string; at: number }

function readLogin(): BrowserLogin | null {
  try {
    const v = JSON.parse(window.localStorage.getItem(LOGIN) ?? 'null') as {
      user?: unknown
      session?: unknown
      at?: unknown
    } | null
    return v && Number.isInteger(v.user) && typeof v.session === 'string'
      ? { user: v.user as number, session: v.session, at: Number.isFinite(v.at) ? (v.at as number) : 0 }
      : null
  } catch {
    return null
  }
}

// 'stale': something newer is known (the answer is not to be acted on); 'unkept': the browser didn't
// keep it (storage refused), which is no reason to stop.
export function rememberLogin(
  user: number,
  session: string | undefined,
  asked = Date.now(),
): 'ok' | 'stale' | 'unkept' {
  const known = readLogin()
  if (known && known.at > asked) return 'stale'
  try {
    window.localStorage.setItem(LOGIN, JSON.stringify({ user, session: session ?? '', at: asked }))
    return 'ok'
  } catch {
    // Not kept: a late sign-out of an older login clears the device as it would have.
    return 'unkept'
  }
}

// The login `session` of `user` is over: it is no longer the browser's, if it was, and an answer
// about it asked before now is stale. Compared first, so a newer login remembered meanwhile stays.
export function forgetLogin(user: number, session: string | undefined): void {
  const now = browserLogin()
  if (!now || now.user !== user || !session || now.session !== session) return
  try {
    window.localStorage.setItem(LOGIN, JSON.stringify({ user, session: '', at: Date.now() }))
  } catch {
    try {
      window.localStorage.removeItem(LOGIN)
    } catch {
      // Not removed: a later cleanup of an older login is shielded by it, and leaves the device.
    }
  }
}

// Whether `user`'s login `session` is still what this browser knows of last: no other login since,
// for this account or another, and not seen to end. True when nothing is known (it couldn't be kept,
// or the server names no login).
export function loginIsCurrent(user: number, session: string | undefined): boolean {
  const known = readLogin()
  if (!session || !known) return true
  return known.user === user && known.session === session
}

// The login the browser holds now, as far as it knows; null when none (or it ended).
export function browserLogin(): BrowserLogin | null {
  const v = readLogin()
  return v && v.session !== '' ? v : null
}
