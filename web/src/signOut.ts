// What a sign-out does on this device once the server has confirmed it, and only then: the push
// subscription, the drafts, the offline database. Everything here is the browser's, shared by its
// tabs, so each step acts only on what is still the departing account's when it runs (another tab
// may have signed someone else in meanwhile).
//
// The sign-out and the person's decision are recorded before anything of it waits
// (offline/pendingSignOut.ts), so a tab that closes in the middle leaves it to the next start, which
// finishes it with the same decision before any account's offline data is opened, and forgets it only
// once the device is verified clear.

import { leavePush } from './notifications/push'
import { pendingSignOuts, settleSignOut } from './offline/pendingSignOut'
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

// Clears the device for `user`, as they decided (`keep`: their database stays; `approved`: the unsent
// records they agreed to lose). Resolves true when it is done and verified (the pending sign-out is
// then forgotten), false when it isn't (it stays recorded, for the next start).
export async function cleanUpAfterSignOut(user: number, keep: boolean, approved: Approval): Promise<boolean> {
  try {
    await bounded(
      (async () => {
        await leavePush()
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

// At the start, once the app knows who is signed in (`current`, or null), and before any account's
// offline data is opened: each sign-out that didn't finish is finished now, with the decision its
// person made. One whose account is signed in again is not to be: what is there is theirs.
export async function finishUnfinishedSignOuts(current: number | null): Promise<void> {
  for (const p of pendingSignOuts()) {
    if (p.user === current) {
      settleSignOut(p.user)
      continue
    }
    await cleanUpAfterSignOut(p.user, p.keep, new Map(p.approved))
  }
}
