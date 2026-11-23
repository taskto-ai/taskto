// Small conveniences kept in this browser only: an unsent message, the app being built, the pane
// a phone showed. They are never what a page depends on (the browser may have no storage, or be
// in a private window, and throws then), so every read and write is allowed to fail, and what
// matters is on the server.

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
// forgets and another account signing in on this browser never finds.
const DRAFT_PREFIXES = ['taskto:builder', 'taskto:message:']
const DRAFTS_OF = 'taskto:drafts-of'

// forgetDrafts removes every unsent message and builder session kept in this browser.
export function forgetDrafts(): void {
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
  if (readState(DRAFTS_OF) !== String(user)) forgetDrafts()
  writeState(DRAFTS_OF, String(user))
}
