import type { Me } from './api'
import { leavePush } from './notifications/push'
import { type PendingSignOut, pendingSignOuts, settleSignOut } from './offline/pendingSignOut'
import { deactivate, lastUser, stillAgreed } from './offline/runtime'
import { type Approval, OfflineStore } from './offline/store'
import { forgetDraftsOf } from './uiState'

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

async function verifiedClear(user: number, approved: Approval): Promise<boolean> {
  if (lastUser()?.id === user) return false
  const names = await globalThis.indexedDB?.databases?.().catch(() => undefined)
  if (names && !names.some((d) => d.name === OfflineStore.databaseName(user))) return true
  const left = await stillAgreed(user, new Map())
  return !left.some((r) => approved.has(r.id))
}

export async function cleanUpAfterSignOut(user: number, keep: boolean, approved: Approval): Promise<boolean> {
  try {
    await bounded(
      (async () => {
        await leavePush(user)
        forgetDraftsOf(user)
        const left = await deactivate(keep ? { purge: false, keep: true, user } : { purge: true, user, approved })
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

export function signOutToResume(me: Me): PendingSignOut | undefined {
  return pendingSignOuts().find((p) => sameLogin(p, me))
}

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
