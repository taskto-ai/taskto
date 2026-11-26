// A person who deletes their own account is signed out at once, and the sign-in screen says why
// (docs/api.md#deleting-an-account): the date the account is purged is handed over in the tab's
// session storage, which is empty again once the notice has been shown.

const KEY = 'taskto.accountDeleted'

// Told to a sign-in screen that is already showing: a request refused a moment before the deletion
// was answered takes the app there first, and that screen took the date from storage before it was
// there.
export const DELETION_EVENT = 'taskto:account-deleted'

export function rememberDeletion(purgeAt: string): void {
  try {
    window.sessionStorage.setItem(KEY, purgeAt)
  } catch {
    // No storage (a private window): the sign-in screen just doesn't explain.
  }
  window.dispatchEvent(new Event(DELETION_EVENT))
}

// The purge date to tell the person, once.
export function takeDeletion(): string | null {
  try {
    const at = window.sessionStorage.getItem(KEY)
    window.sessionStorage.removeItem(KEY)
    return at
  } catch {
    return null
  }
}
