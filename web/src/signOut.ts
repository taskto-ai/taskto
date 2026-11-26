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

import type { Me } from './api'
import { leavePush } from './notifications/push'
import { type PendingSignOut, pendingSignOuts, settleSignOut } from './offline/pendingSignOut'
import { deactivate, lastUser, stillAgreed } from './offline/runtime'
import { type Approval, OfflineStore } from './offline/store'
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
// left is only what they did not agree to lose (made after they were asked), kept for the account.
async function verifiedClear(user: number, approved: Approval): Promise<boolean> {
  if (lastUser()?.id === user) return false
  // Gone altogether (the usual end). Asked without opening it, since opening one makes it again.
  const names = await globalThis.indexedDB?.databases?.().catch(() => undefined)
  if (names && !names.some((d) => d.name === OfflineStore.databaseName(user))) return true
  const left = await stillAgreed(user, new Map())
  return !left.some((r) => approved.has(r.id))
}

// Clears the device for `user` once their login is over, as they decided (`keep`: their database
// stays; `approved`: the unsent records they agreed to lose). Resolves true when it is done and
// verified (the pending sign-out is then forgotten), false when this device couldn't be cleared (it
// stays recorded, for the next start). Nothing here asks the server, so a false is this device's
// storage, never the connection.
export async function cleanUpAfterSignOut(user: number, keep: boolean, approved: Approval): Promise<boolean> {
  try {
    await bounded(
      (async () => {
        await leavePush(user)
        forgetDraftsOf(user)
        const left = await deactivate(keep ? { purge: false, keep: true, user } : { purge: true, user, approved })
        // Records made since the person was asked (another tab of the account): not theirs to lose
        // unseen, so the database stays, listed for the account's next sign-in to deal with.
        if (left.length > 0) await deactivate({ purge: false, keep: true, user })
      })(),
    )
    if (!(await bounded(verifiedClear(user, approved)))) return false
    settleSignOut(user)
    return true
  } catch {
    return false
  }
}

const sameLogin = (p: PendingSignOut, me: Me) => p.user === me.user.id && p.session !== '' && p.session === me.session

// The unresolved sign-out of the login this browser holds now (`me`), which the app takes up again
// rather than signing in; undefined when there is none.
export function signOutToResume(me: Me): PendingSignOut | undefined {
  return pendingSignOuts().find((p) => sameLogin(p, me))
}

// At the start, once the app knows who is signed in (`me`, or null), and before any account's
// offline data is opened. A sign-out of the login this browser holds is left to the app to take up
// (signOutToResume). One whose account is signed in again with another login is over: that newer
// login is the person's, and so is what is on the device. Any other is finished now, with the decision
// its person made; a device that can't be cleared keeps the record (that account isn't opened offline)
// and the start goes on for whoever is signing in.
export async function finishUnfinishedSignOuts(me: Me | null): Promise<void> {
  for (const p of pendingSignOuts()) {
    if (me && sameLogin(p, me)) continue
    if (me && p.user === me.user.id) {
      settleSignOut(p.user)
      continue
    }
    await cleanUpAfterSignOut(p.user, p.keep, new Map(p.approved))
  }
}
