// A person who deletes their own account is signed out at once, and the sign-in screen says why
// (docs/api.md#deleting-an-account): the date the account is purged is handed over in the tab's
// session storage, bound to the account that was deleted: `{ user, purgeAt }`.
//
// Who may be told, and when:
// - It is written only by the deletion's own answer (`rememberDeletion`), and only if no other
//   account has signed in on this page since the request was made (`startDeletion`): an answer that
//   arrives after another account signed in is for a browser that has moved on, and is dropped.
// - It is shown only by the sign-in screen, which exists only while nobody is signed in.
// - Reading it never removes it: a screen may be built and thrown away before the notice is drawn
//   (its login methods still loading, an error and a retry, a reload, React's double render in
//   development). The screen that has drawn it says so (`deletionShown`).
// - Any account completing a sign-in on this page (`noteSignIn`) drops a notice still waiting,
//   without showing it. A notice that was waiting while another account was typing its password or
//   its code was kept out of that account's way, and that sign-in shows the browser has moved on from
//   the deleted account: it is not told to the account after either.
//
// Storage may refuse any operation, each on its own. The notice is kept in memory too, so a refused
// write is still told to a sign-in screen of this page, and what has been shown or dropped is marked
// in memory, so a refused removal never tells it a second time. The stored copy is overwritten with
// a "shown" marker where it can be, and removed only if it can't.

const KEY = 'taskto.accountDeleted'

// Told to a sign-in screen that is already showing: a request refused a moment before the deletion
// was answered takes the app there first, and that screen read the date from storage before it was
// there.
export const DELETION_EVENT = 'taskto:account-deleted'

type Notice = { user: number; purgeAt: string; shown?: boolean }

// The deletion request of one account, remembered with the sign-ins this page had seen when it was made.
export type DeletionRequest = { user: number; epoch: number }

let memory: Notice | null = null
// The notice that has been shown or dropped: neither memory nor storage tells it again.
let spent: string | null = null
// Which account signed in last on this page, and how many different ones have since it began.
let signedIn: number | null = null
let epoch = 0

const keyOf = (n: Notice) => `${n.user}|${n.purgeAt}`

function stored(): Notice | null {
  try {
    const raw = window.sessionStorage.getItem(KEY)
    if (!raw) return null
    const n = JSON.parse(raw) as Partial<Notice> | null
    if (n && typeof n.user === 'number' && typeof n.purgeAt === 'string') {
      return { user: n.user, purgeAt: n.purgeAt, shown: n.shown === true }
    }
  } catch {
    // No storage, or something else than a notice in it.
  }
  return null
}

// Overwrites the stored copy with a marker rather than removing it: a removal the browser refuses
// would leave the notice there to be told again.
function retire(n: Notice) {
  spent = keyOf(n)
  memory = null
  try {
    window.sessionStorage.setItem(KEY, JSON.stringify({ user: n.user, purgeAt: n.purgeAt, shown: true }))
    return
  } catch {
    // Not writable: try to remove it; the mark in memory covers a removal that fails too.
  }
  try {
    window.sessionStorage.removeItem(KEY)
  } catch {
    // Nothing more to do.
  }
}

// An account signed in on this page (App calls it for every sign-in). Any notice still waiting is
// dropped, never shown.
export function noteSignIn(user: number): void {
  if (signedIn !== user) epoch++
  signedIn = user
  const waiting = current()
  if (waiting && !waiting.shown && keyOf(waiting) !== spent) retire(waiting)
}

// Called when the deletion request is made: who made it, and which sign-ins this page had seen.
export function startDeletion(user: number): DeletionRequest {
  return { user, epoch }
}

// The deletion was answered: the notice is kept for the sign-in screen, unless another account has
// signed in on this page since the request was made.
export function rememberDeletion(request: DeletionRequest, purgeAt: string): void {
  if (request.epoch !== epoch || (signedIn !== null && signedIn !== request.user)) return
  const notice: Notice = { user: request.user, purgeAt }
  spent = null
  memory = notice
  try {
    window.sessionStorage.setItem(KEY, JSON.stringify(notice))
  } catch {
    // No storage (a private window): this page's sign-in screen is told from memory; a reload isn't.
    try {
      window.sessionStorage.removeItem(KEY) // an older notice left there is not this one
    } catch {
      // Nothing more to do.
    }
  }
  window.dispatchEvent(new Event(DELETION_EVENT))
}

// The notice as this page knows it. One read from storage is kept in memory: retiring it must not
// need a second successful read (a read that fails between showing and retiring it).
function current(): Notice | null {
  if (!memory) memory = stored()
  return memory
}

// The purge date to tell the person, if there is one. It stays until `deletionShown` or a sign-in.
export function peekDeletion(): string | null {
  const n = current()
  if (!n || n.shown || keyOf(n) === spent) return null
  return n.purgeAt
}

// The notice has been drawn: it is told no more.
export function deletionShown(): void {
  const n = current()
  if (n) retire(n)
}

// For tests: this page as it was before anything happened.
export function resetDeletion(): void {
  memory = null
  spent = null
  signedIn = null
  epoch = 0
  try {
    window.sessionStorage.removeItem(KEY)
  } catch {
    // Nothing kept that could be removed.
  }
}
