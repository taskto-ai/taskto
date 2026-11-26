// A person who deletes their own account is signed out at once, and the sign-in screen says why
// (docs/api.md#deleting-an-account): the date the account is purged is handed over in the tab's
// session storage, which is empty again once the notice has been shown.

const KEY = 'taskto.accountDeleted'

export function rememberDeletion(purgeAt: string): void {
  try {
    window.sessionStorage.setItem(KEY, purgeAt)
  } catch {
    // No storage (a private window): the sign-in screen just doesn't explain.
  }
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
