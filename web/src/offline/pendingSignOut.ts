import type { Approval, Seen } from './store'

const KEY = 'taskto:sign-out-pending'

export type SignOutPhase = 'requested' | 'confirmed' | 'backed'

export type PendingSignOut = {
  user: number
  session: string
  phase: SignOutPhase
  keep: boolean
  approved: [string, Seen][]
  unanswered?: boolean
  discardUnreadable?: boolean
  at?: number
}

const isSeen = (v: unknown): v is Seen =>
  !!v && typeof v === 'object' && Number.isInteger((v as Seen).version) && typeof (v as Seen).operation === 'string'

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
      } catch {}
    }
  } catch {
    return legacy()
  }
  const has = (p: PendingSignOut) => own.some((o) => o.user === p.user && o.session === p.session)
  return [...legacy().filter((p) => !has(p)), ...own]
}

function put(user: number, session: string, p: PendingSignOut | null): boolean {
  try {
    if (p) window.localStorage.setItem(keyOf(user, session), JSON.stringify(p))
    else window.localStorage.removeItem(keyOf(user, session))
  } catch {
    return false
  }
  try {
    const old = legacy()
    const rest = old.filter((r) => !(r.user === user && r.session === session))
    if (rest.length !== old.length) {
      if (rest.length === 0) window.localStorage.removeItem(KEY)
      else window.localStorage.setItem(KEY, JSON.stringify(rest))
    }
  } catch {}
  return true
}

export function pendingSignOuts(): PendingSignOut[] {
  return read()
}

export function pendingSignOutOf(user: number): PendingSignOut | undefined {
  return read().find((p) => p.user === user)
}

export function recordSignOut(
  user: number,
  session: string,
  keep: boolean,
  approved: Approval,
  discardUnreadable = false,
  at = Date.now(),
): number {
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
    at,
  })
  return at
}

export type SignOutDecision = { keep: boolean; approved: Approval; discardUnreadable?: boolean; at?: number }

export function ensureSignOut(
  user: number,
  session: string,
  d: SignOutDecision,
  phase: SignOutPhase = 'requested',
): void {
  const held = read().find((p) => p.user === user && p.session === session)
  if (held && held.phase !== 'backed') return
  put(user, session, {
    user,
    session,
    phase,
    keep: d.keep,
    approved: [...d.approved],
    ...(d.discardUnreadable ? { discardUnreadable: true } : {}),
    ...(held?.unanswered ? { unanswered: true } : {}),
    ...(held?.at !== undefined ? { at: held.at } : d.at !== undefined ? { at: d.at } : {}),
  })
}

function change(
  user: number,
  session: string,
  to: (p: PendingSignOut) => PendingSignOut | null,
): 'none' | 'written' | 'failed' {
  const p = read().find((r) => r.user === user && r.session === session)
  if (!p) return 'none'
  return put(user, session, to(p)) ? 'written' : 'failed'
}

const LOGOUT = 'taskto:sign-out-logout:'
const logoutsOf = (user: number, session: string) => `${LOGOUT}${user}:${session}:`

export function beginLogout(user: number, session: string): string {
  const key = `${logoutsOf(user, session)}${Date.now().toString(36)}${Math.random().toString(36).slice(2, 8)}`
  try {
    window.localStorage.setItem(key, String(Date.now()))
  } catch {}
  return key
}

export function endLogout(key: string, user: number, session: string, answered: boolean): void {
  if (!answered && !unansweredSignOut(user, session)) return
  try {
    window.localStorage.removeItem(key)
  } catch {}
}

function logoutsOnTheirWay(user: number, session: string): string[] {
  const keys: string[] = []
  try {
    const prefix = logoutsOf(user, session)
    for (let i = 0; i < window.localStorage.length; i++) {
      const key = window.localStorage.key(i)
      if (key?.startsWith(prefix)) keys.push(key)
    }
  } catch {}
  return keys
}

export function unansweredSignOut(user: number, session: string): boolean {
  return change(user, session, (p) => ({ ...p, unanswered: true })) !== 'failed'
}

export function backFromSignOut(user: number, session: string): void {
  change(user, session, (p) =>
    p.unanswered || logoutsOnTheirWay(user, session).length > 0 ? { ...p, phase: 'backed' } : null,
  )
}

export function backedSignOut(user: number, session: string | undefined): PendingSignOut | undefined {
  return read().find((p) => p.user === user && p.phase === 'backed' && !!session && p.session === session)
}

export function confirmSignOut(user: number, session: string, d?: SignOutDecision): void {
  if (d) ensureSignOut(user, session, d, 'confirmed')
  change(user, session, (p) => ({ ...p, phase: 'confirmed' }))
}

export function settleSignOut(user: number, session?: string): void {
  for (const p of read()) {
    if (p.user !== user || (session !== undefined && p.session !== session)) continue
    put(p.user, p.session, null)
    for (const key of logoutsOnTheirWay(p.user, p.session)) {
      try {
        window.localStorage.removeItem(key)
      } catch {}
    }
  }
}

export function signOutPending(user: number): boolean {
  return read().some((p) => p.user === user && p.phase !== 'backed')
}

const LOGIN = 'taskto:login'

export type BrowserLogin = { user: number; session: string; at: number }

function readLogin(): (BrowserLogin & { ended?: string }) | null {
  try {
    const v = JSON.parse(window.localStorage.getItem(LOGIN) ?? 'null') as {
      user?: unknown
      session?: unknown
      at?: unknown
      ended?: unknown
    } | null
    return v && Number.isInteger(v.user) && typeof v.session === 'string'
      ? {
          user: v.user as number,
          session: v.session,
          at: Number.isFinite(v.at) ? (v.at as number) : 0,
          ...(typeof v.ended === 'string' ? { ended: v.ended } : {}),
        }
      : null
  } catch {
    return null
  }
}

export function rememberLogin(
  user: number,
  session: string | undefined,
  asked = Date.now(),
): 'ok' | 'stale' | 'unkept' {
  const known = readLogin()
  const endedNow = known?.session === '' && known.user === user && !!session && known.ended === session
  if (known && (known.at > asked || (endedNow && known.at === asked))) return 'stale'
  try {
    window.localStorage.setItem(LOGIN, JSON.stringify({ user, session: session ?? '', at: asked }))
    return 'ok'
  } catch {
    return 'unkept'
  }
}

export function forgetLogin(user: number, session: string | undefined): void {
  const now = browserLogin()
  if (!now || now.user !== user || !session || now.session !== session) return
  try {
    window.localStorage.setItem(LOGIN, JSON.stringify({ user, session: '', ended: session, at: Date.now() }))
  } catch {
    try {
      window.localStorage.removeItem(LOGIN)
    } catch {}
  }
}

export function loginIsCurrent(user: number, session: string | undefined): boolean {
  const known = readLogin()
  if (!session || !known) return true
  return known.user === user && known.session === session
}

export function browserLogin(): BrowserLogin | null {
  const v = readLogin()
  return v && v.session !== '' ? v : null
}
