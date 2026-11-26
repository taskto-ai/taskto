// The idempotency key of a press of an app's button (the server's `operation`): 8 to 64 letters,
// digits, - and _.
//
// A submission gets one key, made once, and every press of the same submission while no definite
// answer has come (the connection dropped, a gateway timed out: the person can't know whether it was
// made) sends that key again, so the server changes nothing a second time and queues no second card.
//
// The keys still waiting live here, outside any component: a screen that is drawn again (a refresh
// of the stage, a remount, a new transport) must find the key its predecessor sent, and so must a
// page that was reloaded, which is why they are mirrored in this tab's session storage. They belong
// to the signed-in user and say which app, release and action they were made for: an answer that
// comes after that user signed out, or another signed in, changes nothing of anyone's (it never
// writes one person's pending submissions under the next person's key), and signing out forgets
// them all (uiState.forgetDrafts).

import { ApiError } from '../api'

// The same shape as the renderer's action; only what identifies a submission is read from it.
type Pressed = { name: string; timestamp?: unknown } & Record<string, unknown>

// Only the newest are remembered, so a long session can't grow this without end.
const MAX_PENDING = 50
const STORAGE_PREFIX = 'taskto:operations:'

let user: number | null = null
const stores = new Map<string, Map<string, string>>()

// Whose keys are kept: the signed-in user (null after sign-out; nothing is mirrored then).
export function setOperationUser(id: number | null): void {
  user = id
}

function owner(): string {
  return user === null ? 'anonymous' : String(user)
}

function newOperation(): string {
  return globalThis.crypto?.randomUUID?.() ?? `op-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`
}

function pending(): Map<string, string> {
  const who = owner()
  let map = stores.get(who)
  if (!map) {
    map = new Map()
    try {
      const raw = user === null ? null : window.sessionStorage.getItem(STORAGE_PREFIX + who)
      for (const [key, operation] of raw ? (JSON.parse(raw) as [string, string][]) : []) {
        if (typeof key === 'string' && typeof operation === 'string') map.set(key, operation)
      }
    } catch {
      // No storage, or an unreadable copy: the keys are kept in memory only.
    }
    stores.set(who, map)
  }
  return map
}

function mirror(map: Map<string, string>, who: number | null = user): void {
  // Only the account's current map: one that was forgotten (signed out, even if the same person
  // signed in again since) is never written over the new one.
  if (user === null || who !== user || stores.get(String(who)) !== map) return
  try {
    window.sessionStorage.setItem(STORAGE_PREFIX + String(who), JSON.stringify([...map]))
  } catch {
    // Not kept past this page: the keys still hold while it is open.
  }
}

// The value with its object keys in order, so the same submission is the same text however the
// renderer ordered it.
function canonical(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonical)
  if (value && typeof value === 'object') {
    return Object.fromEntries(
      Object.entries(value as Record<string, unknown>)
        .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
        .map(([k, v]) => [k, canonical(v)]),
    )
  }
  return value
}

// What identifies one submission: where it goes (the app's routes, the release it runs from) and
// the action as the person sent it, without the renderer's per-press timestamp.
export function submissionKey(scope: string, action: Pressed): string {
  const { timestamp: _timestamp, ...rest } = action
  return JSON.stringify([scope, action.name, canonical(rest)])
}

// Whether the server answered for good: the request was made (2xx is not an error here) or was
// refused with a code of its own. A network error, a 5xx, a gateway's answer without a code, a
// timeout or a rate limit leaves the outcome unknown: the request may have been made.
export function isDefinitive(error: unknown): boolean {
  if (!(error instanceof ApiError)) return false
  if (error.status < 400 || error.status >= 500) return false
  if (error.status === 408 || error.status === 425 || error.status === 429) return false
  // Refused with the server's own code. (A key the server says is invalid or was used for another
  // request can never succeed, so a new one is made too.)
  return error.code !== undefined
}

// Sends an action under its submission's key, made once and kept until the answer is definite.
export async function withOperation<T>(
  scope: string,
  action: Pressed,
  send: (operation: string) => Promise<T>,
): Promise<T> {
  const who = user
  const map = pending()
  const sent = submissionKey(scope, action)
  let operation = map.get(sent)
  if (operation === undefined) {
    operation = newOperation()
    map.set(sent, operation)
    if (map.size > MAX_PENDING) map.delete(map.keys().next().value as string)
    mirror(map)
  }
  try {
    const answer = await send(operation)
    map.delete(sent)
    mirror(map, who)
    return answer
  } catch (e) {
    if (isDefinitive(e)) {
      map.delete(sent)
      mirror(map, who)
    }
    throw e
  }
}

// Forgets the keys of everyone but `user`, here and in this tab's session storage: when an account
// claims this browser (uiState.claimDrafts), whatever another account left pending in this tab
// (another tab signed it out and someone else in, this one was reloaded) is not kept.
export function keepOperationsOf(user: number): void {
  for (const who of [...stores.keys()]) if (who !== String(user)) stores.delete(who)
  try {
    for (let i = window.sessionStorage.length - 1; i >= 0; i--) {
      const key = window.sessionStorage.key(i)
      if (key?.startsWith(STORAGE_PREFIX) && key !== STORAGE_PREFIX + String(user))
        window.sessionStorage.removeItem(key)
    }
  } catch {
    // Nothing kept.
  }
}

// Forgets the keys of `user` only, here and in this tab's session storage: their sign-out confirmed,
// or their account deleted (uiState.forgetDraftsOf). Another account's, signed in since, stay.
export function forgetOperationsOf(user: number): void {
  stores.delete(String(user))
  try {
    window.sessionStorage.removeItem(STORAGE_PREFIX + String(user))
  } catch {
    // Nothing kept.
  }
}

// Forgets every key, here and in this tab's session storage: when the person signs out or another
// account claims this browser (uiState.forgetDrafts), and between tests.
export function forgetOperations(): void {
  stores.clear()
  try {
    for (let i = window.sessionStorage.length - 1; i >= 0; i--) {
      const key = window.sessionStorage.key(i)
      if (key?.startsWith(STORAGE_PREFIX)) window.sessionStorage.removeItem(key)
    }
  } catch {
    // Nothing kept.
  }
}
