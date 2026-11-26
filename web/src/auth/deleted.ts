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

// Whether this tab's person has just deleted their account, without taking the date: the app
// forgets what the device kept of the account when the session ends for that reason, and not
// when it ends for any other (an ended session leaves the queue for the next sign-in to send).
export function deletionNoted(): boolean {
  try {
    return window.sessionStorage.getItem(KEY) !== null
  } catch {
    return false
  }
}
