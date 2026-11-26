// A person who deletes their own account is signed out at once, and the sign-in screen says why
// (docs/api.md#deleting-an-account): the date the account is purged is handed over in the tab's
// session storage. Reading it does not use it up: the screen that shows the notice acknowledges it
// (`ackDeletion`) once the notice is on the screen and the page is visible, so a reload before that
// (a slow or failed fetch of the login methods, a tab opened in the background) still shows it.

const KEY = 'taskto.accountDeleted'

// Told to a sign-in screen that is already showing: a request refused a moment before the deletion
// was answered takes the app there first, and that screen took the date from storage before it was
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

// The purge date still to tell the person. It stays until `ackDeletion`.
export function peekDeletion(): string | null {
  try {
    return window.sessionStorage.getItem(KEY) ?? inMemory
  } catch {
    return inMemory
  }
}

// The notice has been shown: it is told no more. Given the date that was shown, a newer deletion
// remembered meanwhile is kept; given none, whatever is kept is dropped.
export function ackDeletion(shown?: string): void {
  if (shown !== undefined && inMemory !== null && inMemory !== shown) return
  inMemory = null
  try {
    const kept = window.sessionStorage.getItem(KEY)
    if (shown === undefined || kept === shown) window.sessionStorage.removeItem(KEY)
  } catch {
    // No storage: only the copy in memory was kept.
  }
}
