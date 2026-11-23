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
