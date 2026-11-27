import { signInSubmitted } from './auth/deleted'

let generation = 0
let user: number | null = null
const listeners = new Set<() => void>()

function changed() {
  generation += 1
  for (const fn of [...listeners]) fn()
}

export function beginSession(id: number): void {
  signInSubmitted()
  user = id
  changed()
}

export function endSession(): void {
  if (user === null) return
  user = null
  changed()
}

export function sessionUser(): number | null {
  return user
}

export function resetSession(): void {
  user = null
  changed()
}

export function onSessionChange(fn: () => void): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

export function sameSession(): () => boolean {
  const at = generation
  return () => generation === at
}

export class SessionEnded extends Error {
  constructor() {
    super('the session ended')
    this.name = 'SessionEnded'
  }
}

export function steps(alive?: { readonly current: boolean }) {
  const same = sameSession()
  const ok = () => same() && (alive === undefined || alive.current)
  return {
    ok,
    check<T>(value: T): T {
      if (!ok()) throw new SessionEnded()
      return value
    },
  }
}
