import type { Approval, Seen } from './store'

const KEY = 'taskto:sign-out-pending'

export type SignOutPhase = 'requested' | 'confirmed'

export type PendingSignOut = {
  user: number
  session: string
  phase: SignOutPhase
  keep: boolean
  approved: [string, Seen][]
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
  } catch {}
}

export function pendingSignOuts(): PendingSignOut[] {
  return read()
}

export function pendingSignOutOf(user: number): PendingSignOut | undefined {
  return read().find((p) => p.user === user)
}

export function recordSignOut(user: number, session: string, keep: boolean, approved: Approval): void {
  write([
    ...read().filter((p) => p.user !== user),
    { user, session, phase: 'requested', keep, approved: [...approved] },
  ])
}

export function confirmSignOut(user: number): void {
  write(read().map((p) => (p.user === user ? { ...p, phase: 'confirmed' } : p)))
}

export function settleSignOut(user: number): void {
  write(read().filter((p) => p.user !== user))
}

export function signOutPending(user: number): boolean {
  return read().some((p) => p.user === user)
}
