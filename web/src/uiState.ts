// Small conveniences kept in this browser only: an unsent message, the app being built, the pane
// a phone showed. They are never what a page depends on (the browser may have no storage, or be
// in a private window, and throws then), so every read and write is allowed to fail, and what
// matters is on the server.

import { setOperationUser } from './apps/operations'

export function readState(key: string): string | null {
  try {
    return window.localStorage.getItem(key)
  } catch {
    return null
  }
}

// null (or the empty string) forgets the key.
export function writeState(key: string, value: string | null): void {
  try {
    if (value) window.localStorage.setItem(key, value)
    else window.localStorage.removeItem(key)
  } catch {
    // Not kept: the page works as it did.
  }
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

// What a person types and hasn't sent yet is theirs: the keys that hold it, which signing out
// forgets and another account signing in on this browser never finds. Every such key names its
// user (draftKey), so one account's tab can never write where another's reads: a tab left open
// by someone who has since signed out writes to a key of its own user, which nobody else opens, and
// it stops writing at all once another account has claimed the browser.
const DRAFT_PREFIXES = ['taskto:builder', 'taskto:message:']
const DRAFTS_OF = 'taskto:drafts-of'

// The user this page's drafts belong to: who signed in here (claimDrafts), none after sign-out.
let owner: number | null = null

// draftKey is the key of one of the signed-in user's drafts: kind is 'builder', 'builder-pane',
// 'builder-start' or 'message', and the parts say which one. Without a signed-in user there is
// none: nothing is read or written.
export function draftKey(kind: string, ...parts: (string | number)[]): string | null {
  return owner === null ? null : [`taskto:${kind}`, owner, ...parts].join(':')
}

// Whether this page's user is still the one who has the browser's drafts: another tab may have
// signed in another account since, which forgot this one's.
function mine(): boolean {
  return owner !== null && claimed() === String(owner)
}

// Who has the browser's drafts, as the browser says, so what another tab did counts.
function claimed(): string | null {
  return readState(DRAFTS_OF)
}

export function readDraft(key: string | null): string | null {
  return key !== null && mine() ? readState(key) : null
}

export function readDraftJson<T>(key: string | null): T | null {
  return key !== null && mine() ? readJson<T>(key) : null
}

// A draft is written only while this page's user still has the browser (see mine).
export function writeDraft(key: string | null, value: string | null): void {
  if (key !== null && mine()) writeState(key, value)
}

// forgetDrafts removes every unsent message and builder session kept in this browser, and this
// page's claim on them.
export function forgetDrafts(): void {
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
  } catch {
    // Nothing kept, nothing to forget.
  }
}

// claimDrafts keeps the drafts in this browser for user, forgetting them first if they were
// someone else's.
export function claimDrafts(user: number): void {
  if (claimed() !== String(user)) forgetDrafts()
  owner = user
  setOperationUser(user)
  writeState(DRAFTS_OF, String(user))
}
