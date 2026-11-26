// The signed-in session this page is in: whose it is, and which one (a generation, one more each time
// a session begins or ends). It is recorded the moment it changes, never later, so no answer that
// comes after can act in a session that isn't its own:
//
// - begun by App when someone signs in, before the shell is drawn (its first effects are already
//   in the new session);
// - ended in api() when the server says the session is gone (401), before the request's caller hears
//   of it; at the start of signing out; and when another account signs in, or this one signs out, in
//   another tab (App).
//
// Work a session started (a save, a create, steps one after another) takes `sameSession()`, or
// `steps()`, when it starts, and asks it after each wait: one that comes back after the session
// ended shows nothing, opens nothing, writes nothing and sends nothing more, as the browser may be
// the next person's by then. While a session lasts, api() names its account on every request that
// changes something (X-Taskto-Expected-User), so a request this page sends can't change another
// account's data if the browser's cookie became someone else's meanwhile: the server refuses it.
//
// It is apart from whether the screen that started some work is still mounted (mounted.ts): the
// shell can go and come back within one session (opening a push's notification), and what belongs
// to the account (a language being saved, a workspace made) goes on through that.

let generation = 0
let user: number | null = null
// The account of a session that ended in a sign-out not finished yet (see `leaving`).
let departing: number | null = null
const listeners = new Set<() => void>()

function changed() {
  generation += 1
  for (const fn of [...listeners]) fn()
}

// Someone signed in: their session begins (a new one, even for the same person again).
export function beginSession(id: number): void {
  user = id
  departing = null
  changed()
}

// The session ended. Nothing to do when none is going on.
export function endSession(): void {
  departing = null
  if (user === null) return
  user = null
  changed()
}

// The session ends because its person is signing out, and the sign-out isn't finished: what the
// page still sends meanwhile (the shell is kept, hidden, in case the server doesn't confirm it) names
// the departing account, so it is refused if the browser became someone else's in another tab. It
// lasts until the sign-out ends one way or another (endSession, or beginSession on the way back).
export function leaving(id: number): void {
  endSession()
  departing = id
}

// The account of the session going on, or null.
export function sessionUser(): number | null {
  return user
}

// The account a request that changes something names (api.ts): the session's, or the departing one's
// while its sign-out finishes.
export function requestAccount(): number | null {
  return user ?? departing
}

// For tests: no session, and a new generation, so nothing of one test's session reaches the next.
export function resetSession(): void {
  user = null
  departing = null
  changed()
}

// Told each time a session begins or ends (the toasts go then).
export function onSessionChange(fn: () => void): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

// Whether the session is still the one it was when this was called.
export function sameSession(): () => boolean {
  const at = generation
  return () => generation === at
}

// The session ended (or the screen went) while an action was waiting: it stops there.
export class SessionEnded extends Error {
  constructor() {
    super('the session ended')
    this.name = 'SessionEnded'
  }
}

// The steps of one action, all in the session it started in and, with `alive` (useMounted()), while
// the screen that started it is there. `check(value)` passes value on, or throws SessionEnded once
// either is gone; put it after every wait. `ok()` says the same without throwing.
//
//   const s = steps(mounted)
//   try {
//     const made = s.check(await api(...))
//     s.check(await api(...))
//     navigate(...)
//   } catch (e) {
//     if (!s.ok()) return
//     setError(errorText(e))
//   }
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
