// A person who deletes their own account is signed out at once, and the sign-in screen says why
// (docs/api.md#deleting-an-account): the date the account is purged is handed over in the tab's
// session storage.
//
// Reading the date never removes it: a screen may be built and thrown away before the notice is
// drawn (its login methods still loading, an error and a retry, a reload, React's double render in
// development), and what was taken then would never be shown. The date is removed by the screen that
// has drawn it (`clearDeletion`), and by any sign-in (`App`), so that another account signing in
// here never finds it.

const KEY = 'taskto.accountDeleted'

// Told to a sign-in screen that is already showing: a request refused a moment before the deletion
// was answered takes the app there first, and that screen read the date from storage before it was
// there.
export const DELETION_EVENT = 'taskto:account-deleted'

// Kept in memory too, for a browser whose storage throws: the notice is then told to a sign-in screen
// of this page, and not to one after a reload.
let inMemory: string | null = null

export function rememberDeletion(purgeAt: string): void {
  inMemory = purgeAt
  try {
    window.sessionStorage.setItem(KEY, purgeAt)
  } catch {
    // No storage (a private window): the sign-in screen just doesn't explain.
  }
  window.dispatchEvent(new Event(DELETION_EVENT))
}

// The purge date to tell the person, if there is one. It stays until `clearDeletion`.
export function peekDeletion(): string | null {
  if (inMemory) return inMemory
  try {
    return window.sessionStorage.getItem(KEY)
  } catch {
    return null
  }
}

// The notice has been drawn, or the person it was for is no longer the one here: it is told no more.
export function clearDeletion(): void {
  inMemory = null
  try {
    window.sessionStorage.removeItem(KEY)
  } catch {
    // Nothing kept that could be removed.
  }
}
