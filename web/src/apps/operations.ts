import { ApiError } from '../api'

type Pressed = { name: string; timestamp?: unknown } & Record<string, unknown>

const MAX_PENDING = 50
const STORAGE_PREFIX = 'taskto:operations:'

let user: number | null = null
const stores = new Map<string, Map<string, string>>()

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
    } catch {}
    stores.set(who, map)
  }
  return map
}

function mirror(map: Map<string, string>, who: number | null = user): void {
  if (user === null || who !== user || stores.get(String(who)) !== map) return
  try {
    window.sessionStorage.setItem(STORAGE_PREFIX + String(who), JSON.stringify([...map]))
  } catch {}
}

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

export function submissionKey(scope: string, action: Pressed): string {
  const { timestamp: _timestamp, ...rest } = action
  return JSON.stringify([scope, action.name, canonical(rest)])
}

export function isDefinitive(error: unknown): boolean {
  if (!(error instanceof ApiError)) return false
  if (error.status < 400 || error.status >= 500) return false
  if (error.status === 408 || error.status === 425 || error.status === 429) return false
  return error.code !== undefined
}

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

export function keepOperationsOf(user: number): void {
  for (const who of [...stores.keys()]) if (who !== String(user)) stores.delete(who)
  try {
    for (let i = window.sessionStorage.length - 1; i >= 0; i--) {
      const key = window.sessionStorage.key(i)
      if (key?.startsWith(STORAGE_PREFIX) && key !== STORAGE_PREFIX + String(user))
        window.sessionStorage.removeItem(key)
    }
  } catch {}
}

export function forgetOperationsOf(user: number): void {
  stores.delete(String(user))
  try {
    window.sessionStorage.removeItem(STORAGE_PREFIX + String(user))
  } catch {}
}

export function forgetOperations(): void {
  stores.clear()
  try {
    for (let i = window.sessionStorage.length - 1; i >= 0; i--) {
      const key = window.sessionStorage.key(i)
      if (key?.startsWith(STORAGE_PREFIX)) window.sessionStorage.removeItem(key)
    }
  } catch {}
}
