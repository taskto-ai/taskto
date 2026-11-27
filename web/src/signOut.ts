// What a sign-out does on this device once the server has confirmed it, and only then: the push
// subscription, the drafts, the offline database. Everything here is the browser's, shared by its
// tabs, so each step acts only on what is still the departing account's when it runs (another tab
// may have signed someone else in meanwhile).
//
// The sign-out, its login and the person's decision are recorded before anything of it waits
// (offline/pendingSignOut.ts). A start that finds one unresolved takes it up: if this browser still
// holds that login, the sign-out goes on (the app shows it, signOutToResume); if the account is
// signed in again with a newer login, what is there is theirs and the record goes; otherwise the login
// is over for this browser and the device is cleared as the person decided. It is forgotten only once
// the device is verified clear, or when that same login cancels it (Back) or a newer one replaced it.
// A cleanup that comes after a newer login of the same account started here (a logout sent before
// it, answered after) touches nothing: the drafts, the database and the push subscription are that
// login's now.

import type { Me } from './api'
import { leavePush } from './notifications/push'
import {
  browserLogin,
  forgetLogin,
  loginIsCurrent,
  type PendingSignOut,
  pendingSignOuts,
  rememberLogin,
  settleSignOut,
} from './offline/pendingSignOut'
import { deactivate, lastUser, unsentState } from './offline/runtime'
import { type Approval, databaseExists } from './offline/store'
import { forgetDraftsOf } from './uiState'

// The longest a step of signing out waits: the server's answer, then this device's cleanup.
export const SIGN_OUT_WAIT_MS = 15_000

function bounded<T>(work: Promise<T>, ms = SIGN_OUT_WAIT_MS): Promise<T> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('timed out')), ms)
    work.then(
      (v) => {
        clearTimeout(timer)
        resolve(v)
      },
      (e) => {
        clearTimeout(timer)
        reject(e)
      },
    )
  })
}

// Whether the device holds nothing more of `user`'s that the sign-out was to remove: no unsent record
// the person agreed to lose, and no marker that would let an offline start open the account. What is
// left is only what they did not agree to lose (made after they were asked), kept for the account
// with the database. (A database the cleanup retired was emptied as it was, so one whose deletion
// didn't follow holds nothing of theirs.) Not verified: a database that can't be read, and one the
// person chose to discard whatever it held that is still there.
async function verifiedClear(user: number, { keep, approved, discardUnreadable }: Decision): Promise<boolean> {
  if (lastUser()?.id === user) return false
  // A browser with no IndexedDB holds no database; one the person chose to keep is theirs.
  if (typeof indexedDB === 'undefined' || keep) return true
  // Gone altogether (the usual end), asked without leaving one made by asking.
  const present = await databaseExists(user)
  if (present === false) return true
  // One they chose to discard whatever it held is verified gone, or not at all.
  if (discardUnreadable) return false
  const { left, unreadable } = await unsentState(user, new Map())
  return !unreadable && !left.some((r) => approved.has(r.id))
}

// Whether the browser started with a newer login of `user`'s than `session` since, which isn't known
// to be over (`forgetLogin` lets go of one that ended): the device is that login's then.
// A login learned before this sign-out was recorded is not newer than it, whatever its session.
function newerLogin(user: number, session: string): boolean {
  const now = browserLogin()
  if (!session || now?.user !== user || now.session === session) return false
  const recorded = pendingSignOuts().find((p) => p.user === user && p.session === session)?.at ?? 0
  return now.at >= recorded
}

// What the person decided when the sign-out became certain.
export type Decision = {
  // Their database stays.
  keep: boolean
  // The unsent records they agreed to lose, as they were shown.
  approved: Approval
  // They were told their database couldn't be read, and chose to discard it.
  discardUnreadable?: boolean
}

// Clears the device for `user` once their login `session` is over, as they decided. Resolves true
// when it is done and verified (the pending sign-out is then forgotten), false when this device
// couldn't be cleared (it stays recorded, for the next start). Nothing here asks the server, so a
// false is this device's storage, never the connection.
export async function cleanUpAfterSignOut(
  user: number,
  session: string,
  { keep, approved, discardUnreadable = false }: Decision,
): Promise<boolean> {
  // A newer login of the account started here since: nothing on the device is this sign-out's.
  // Asked before every step, and again after every wait inside one (`still`): the push subscription,
  // the drafts, the database (whose deletion is one at a time with opening it, and removes only the
  // database it retired: store.ts), the marker. Only this login's record is ever settled.
  const still = () => !newerLogin(user, session)
  const superseded = () => {
    if (still()) return false
    settleSignOut(user, session)
    return true
  }
  if (superseded()) return true
  // This login is over: it shields nothing from here on.
  forgetLogin(user, session)
  try {
    // The database couldn't be read when it was to go: kept, and not verified clear whatever a later
    // read says (it was never emptied), so the person is told and a later start finishes it.
    let unreadable = false
    await bounded(
      (async () => {
        await leavePush(user, still)
        if (superseded()) return
        forgetDraftsOf(user)
        const left = await deactivate(
          keep ? { purge: false, keep: true, user, still } : { purge: true, user, approved, discardUnreadable, still },
        )
        if (superseded()) return
        // Records made since the person was asked (another tab of the account), or a database that
        // can't be read now: not theirs to lose unseen, so the database stays, listed for the
        // account's next sign-in to deal with.
        if (left === 'unreadable') unreadable = true
        if (left === 'unreadable' || left.length > 0) await deactivate({ purge: false, keep: true, user, still })
      })(),
    )
    if (superseded()) return true
    if (unreadable) return false
    if (!(await bounded(verifiedClear(user, { keep, approved, discardUnreadable })))) return false
    if (superseded()) return true
    settleSignOut(user, session)
    return true
  } catch {
    return false
  }
}

const sameLogin = (p: PendingSignOut, me: Me) => p.user === me.user.id && p.session !== '' && p.session === me.session

// The unresolved sign-out of the login this browser holds now (`me`), which the app takes up again
// rather than signing in; undefined when there is none. Not one the person went back from: they are
// in the app again.
export function signOutToResume(me: Me): PendingSignOut | undefined {
  return pendingSignOuts().find((p) => sameLogin(p, me) && p.phase !== 'backed')
}

// At the start, once the app knows who is signed in (`me`, or null), and before any account's
// offline data is opened. A sign-out of the login this browser holds is left to the app to take up
// (signOutToResume). One whose account is signed in again with another login is over: that newer
// login is the person's, and so is what is on the device. Any other is finished now, with the decision
// its person made; a device that can't be cleared keeps the record (that account isn't opened offline)
// and the start goes on for whoever is signing in.
// The login found is remembered first, for a cleanup of an older one that is still on its way.
// `asked`: when the server was asked who is signed in (a start's answer may come late): a sign-out
// recorded after that is a newer login's, never one this start's login replaced.
// Resolves false when `me` is no longer the browser's login: something newer was learned before
// this answer came (another account or login signed in, or this login ended), or after the waits
// here. Then nothing was done, and the caller claims nothing for it.
export async function finishUnfinishedSignOuts(me: Me | null, asked = Date.now()): Promise<boolean> {
  const remembered = me ? rememberLogin(me.user.id, me.session, asked) : 'ok'
  if (remembered === 'stale') return false
  for (const listed of pendingSignOuts()) {
    // Read again: an earlier cleanup waited, and another tab may have settled or replaced it since.
    const p = pendingSignOuts().find((r) => r.user === listed.user && r.session === listed.session)
    if (!p) continue
    if (me && sameLogin(p, me)) continue
    if (me && p.user === me.user.id) {
      if ((p.at ?? 0) <= asked) settleSignOut(p.user, p.session)
      continue
    }
    // Recorded after this start asked: a login that began since is signing out (its logout may not
    // be answered yet), not one this start found over. Its own tab finishes it, or the next start.
    if ((p.at ?? 0) > asked) continue
    await cleanUpAfterSignOut(p.user, p.session, decisionOf(p))
  }
  // Asked again after those waits: another tab may have signed someone in, or this login out, since.
  if (!me || remembered === 'unkept') return true
  return loginIsCurrent(me.user.id, me.session)
}

export const decisionOf = (p: PendingSignOut): Decision => ({
  keep: p.keep,
  approved: new Map(p.approved),
  discardUnreadable: p.discardUnreadable ?? false,
})
