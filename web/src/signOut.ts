// What a sign-out does on this device once the server has confirmed it, and only then: the push
// subscription, the drafts, the offline database. Everything here is the browser's, shared by its
// tabs, so each step acts only on what is still the departing account's when it runs (another tab
// may have signed someone else in meanwhile). A cleanup that fails or hangs doesn't keep the person
// on the sign-out: it is remembered, and tried again the next time the app starts.

import { leavePush } from './notifications/push'
import { deactivate } from './offline/runtime'
import type { Approval } from './offline/store'
import { forgetDraftsOf } from './uiState'

// The longest a step of signing out waits: the server's answer, then this device's cleanup.
export const SIGN_OUT_WAIT_MS = 15_000

const UNFINISHED = 'taskto:sign-out-unfinished'

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

// `keep`: the person chose to keep the account's database (this version couldn't tell whether it
// holds unsent records). `approved`: the unsent records they agreed to lose, as they were shown.
// Resolves true when everything was cleared, false when something couldn't be (remembered, and tried
// again at the next start).
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
    return true
  } catch {
    remember(user)
    return false
  }
}

function read(): number[] {
  try {
    const v = JSON.parse(window.localStorage.getItem(UNFINISHED) ?? '[]')
    return Array.isArray(v) ? v.filter((n): n is number => Number.isInteger(n)) : []
  } catch {
    return []
  }
}

function write(users: number[]) {
  try {
    if (users.length === 0) window.localStorage.removeItem(UNFINISHED)
    else window.localStorage.setItem(UNFINISHED, JSON.stringify(users))
  } catch {
    // Not remembered: nothing more can be done from here.
  }
}

function remember(user: number) {
  write([...new Set([...read(), user])])
}

// At the start, once the app knows who is signed in (`current`, or null): the cleanups that didn't
// finish are tried again for the accounts that aren't. One that is signed in again keeps what is
// its own, and its entry goes.
export async function finishUnfinishedSignOuts(current: number | null): Promise<void> {
  const users = read()
  if (users.length === 0) return
  const left: number[] = []
  for (const user of users) {
    if (user === current) continue
    try {
      forgetDraftsOf(user)
      await bounded(deactivate({ purge: true, user }))
    } catch {
      left.push(user)
    }
  }
  write(left)
}
