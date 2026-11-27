import { forgetOperations, forgetOperationsOf, keepOperationsOf, setOperationUser } from './apps/operations'

export function readState(key: string): string | null {
  try {
    return window.localStorage.getItem(key)
  } catch {
    return null
  }
}

export function writeState(key: string, value: string | null): void {
  try {
    if (value) window.localStorage.setItem(key, value)
    else window.localStorage.removeItem(key)
  } catch {}
}

export function readJson<T>(key: string): T | null {
  const raw = readState(key)
  if (!raw) return null
  try {
    return JSON.parse(raw) as T
  } catch {
    return null
  }
}

const DRAFT_PREFIXES = ['taskto:builder', 'taskto:message:']
const DRAFTS_OF = 'taskto:drafts-of'

let owner: number | null = null

export function draftKey(kind: string, ...parts: (string | number)[]): string | null {
  return owner === null ? null : [`taskto:${kind}`, owner, ...parts].join(':')
}

function mine(): boolean {
  return owner !== null && claimed() === String(owner)
}

function claimed(): string | null {
  return readState(DRAFTS_OF)
}

export function readDraft(key: string | null): string | null {
  return key !== null && mine() ? readState(key) : null
}

export function readDraftJson<T>(key: string | null): T | null {
  return key !== null && mine() ? readJson<T>(key) : null
}

export function writeDraft(key: string | null, value: string | null): void {
  if (key !== null && mine()) writeState(key, value)
}

export function forgetDrafts(): void {
  forgetOperations()
  clearDrafts()
}

function clearDrafts(): void {
  owner = null
  setOperationUser(null)
  try {
    const keys: string[] = []
    for (let i = 0; i < window.localStorage.length; i++) {
      const key = window.localStorage.key(i)
      if (key && DRAFT_PREFIXES.some((p) => key.startsWith(p))) keys.push(key)
    }
    for (const key of keys) window.localStorage.removeItem(key)
    window.localStorage.removeItem(DRAFTS_OF)
  } catch {}
}

export function forgetDraftsOf(user: number): void {
  if (owner === user) {
    owner = null
    setOperationUser(null)
  }
  forgetOperationsOf(user)
  const kinds = ['builder', 'builder-pane', 'builder-start', 'message']
  const prefixes = kinds.map((k) => `taskto:${k}:${user}:`)
  try {
    const keys: string[] = []
    for (let i = 0; i < window.localStorage.length; i++) {
      const key = window.localStorage.key(i)
      if (key && prefixes.some((p) => key.startsWith(p))) keys.push(key)
    }
    for (const key of keys) window.localStorage.removeItem(key)
    if (claimed() === String(user)) window.localStorage.removeItem(DRAFTS_OF)
  } catch {}
}

export function claimDrafts(user: number): void {
  if (claimed() !== String(user)) clearDrafts()
  keepOperationsOf(user)
  owner = user
  setOperationUser(user)
  writeState(DRAFTS_OF, String(user))
}
