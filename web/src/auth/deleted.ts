// A person who deletes their own account is signed out at once, and the sign-in screen says why
// (docs/api.md#deleting-an-account): the date the account is purged is handed over in the tab's
// session storage. Reading it does not use it up: the screen that shows the notice acknowledges it
// (`ackDeletion`) once the notice is on the screen and the page is visible, so a reload before that
// (a slow or failed fetch of the login methods, a tab opened in the background) still shows it.
//
// The browser may be shared: the notice is for the person who asked, once, and for nobody who signs
// in after them. The sign-in screen can't tell who is typing, so any sign-in submission on this page
// (`signInSubmitted`) ends the notice, and an answer to a deletion request that was sent before
// someone submitted one is not told at all (`beginDeletion`, `rememberDeletion`).

const KEY = 'taskto.accountDeleted'

// Told to a sign-in screen that is already showing: a request refused a moment before the deletion
// was answered takes the app there first, and that screen took the date from storage before it was
// there.
export const DELETION_EVENT = 'taskto:account-deleted'

// The notice is kept in memory too, and what has been shown is remembered there: a browser whose
// storage throws then neither loses the notice (a write that failed) nor tells it again (a removal
// that failed), as long as the page stays. A reload can't do better than the storage does.
let inMemory: string | null = null
const shown = new Set<string>()
// Everything the storage keeps was dropped by an acknowledgement made without a date, whether or
// not it could be read then: a storage that comes back does not tell it.
let droppedAll = false

// How many sign-in submissions this page has seen.
let submissions = 0

function stored(): string | null {
  try {
    // An empty value is what a failed removal falls back to: no notice.
    return window.sessionStorage.getItem(KEY) || null
  } catch {
    return null
  }
}

// Called as a deletion request is sent, with the answer's date given to `rememberDeletion` later.
export function beginDeletion(): number {
  return submissions
}

// The deletion was answered: tell the sign-in screen, unless someone has submitted a sign-in since
// the request was sent (`since`, from `beginDeletion`): the answer is then for a person who is gone,
// and the one at the screen is another. Without `since`, the request is taken as sent just now.
export function rememberDeletion(purgeAt: string, since: number = submissions): void {
  if (since !== submissions) return
  inMemory = purgeAt
  shown.delete(purgeAt)
  droppedAll = false
  try {
    window.sessionStorage.setItem(KEY, purgeAt)
  } catch {
    // No storage (a private window): the sign-in screen just doesn't explain after a reload.
  }
  window.dispatchEvent(new Event(DELETION_EVENT))
}

// The purge date still to tell the person. It stays until `ackDeletion`.
export function peekDeletion(): string | null {
  // What this page remembers goes before the storage: a write that failed must not leave an older
  // date there to be told instead.
  if (inMemory !== null) return inMemory
  const kept = stored()
  return kept !== null && !droppedAll && !shown.has(kept) ? kept : null
}

// The notice has been shown: it is told no more. Given the date that was shown, a newer deletion
// remembered meanwhile is kept; given none, whatever is kept is dropped.
export function ackDeletion(shownDate?: string): void {
  if (shownDate !== undefined && inMemory !== null && inMemory !== shownDate) return
  // What this page remembered came before what the storage kept, so a stored date is older than it
  // (a write that failed left it there): shown along with it.
  const superseded = inMemory !== null
  const gone = shownDate ?? peekDeletion()
  inMemory = null
  if (shownDate === undefined) droppedAll = true
  if (gone !== null) shown.add(gone)
  const kept = stored()
  if (kept !== null && superseded) shown.add(kept)
  if (shownDate !== undefined && !superseded && kept !== null && kept !== shownDate) return
  try {
    window.sessionStorage.removeItem(KEY)
  } catch {
    // Not removable: written over, and `shown` still stops it in this page.
    try {
      window.sessionStorage.setItem(KEY, '')
    } catch {
      // Neither works: only `shown` stops it.
    }
  }
}

// Someone submitted a sign-in form on this page (a login, its code, setup, an invite, a reset): the
// person at the screen is not necessarily the one who deleted their account, so the notice ends here
// and an answer still on its way to be told is refused.
export function signInSubmitted(): void {
  submissions++
  ackDeletion()
}
