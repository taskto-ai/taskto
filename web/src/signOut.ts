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

async function verifiedClear(user: number, { keep, approved, discardUnreadable }: Decision): Promise<boolean> {
  if (lastUser()?.id === user) return false
  if (typeof indexedDB === 'undefined' || keep) return true
  const present = await databaseExists(user)
  if (present === false) return true
  if (discardUnreadable) return false
  const { left, unreadable } = await unsentState(user, new Map())
  return !unreadable && !left.some((r) => approved.has(r.id))
}

function newerLogin(user: number, session: string): boolean {
  const now = browserLogin()
  if (!session || now?.user !== user || now.session === session) return false
  const recorded = pendingSignOuts().find((p) => p.user === user && p.session === session)?.at ?? 0
  return now.at >= recorded
}

export type Decision = {
  keep: boolean
  approved: Approval
  discardUnreadable?: boolean
}

export async function cleanUpAfterSignOut(
  user: number,
  session: string,
  { keep, approved, discardUnreadable = false }: Decision,
): Promise<boolean> {
  const still = () => !newerLogin(user, session)
  const superseded = () => {
    if (still()) return false
    settleSignOut(user, session)
    return true
  }
  if (superseded()) return true
  forgetLogin(user, session)
  try {
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

export function signOutToResume(me: Me): PendingSignOut | undefined {
  return pendingSignOuts().find((p) => sameLogin(p, me) && p.phase !== 'backed')
}

export async function finishUnfinishedSignOuts(me: Me | null, asked = Date.now()): Promise<boolean> {
  const remembered = me ? rememberLogin(me.user.id, me.session, asked) : 'ok'
  if (remembered === 'stale') return false
  for (const listed of pendingSignOuts()) {
    const p = pendingSignOuts().find((r) => r.user === listed.user && r.session === listed.session)
    if (!p) continue
    if (me && sameLogin(p, me)) continue
    if (me && p.user === me.user.id) {
      if ((p.at ?? 0) <= asked) settleSignOut(p.user, p.session)
      continue
    }
    if ((p.at ?? 0) > asked) continue
    await cleanUpAfterSignOut(p.user, p.session, decisionOf(p))
  }
  if (!me || remembered === 'unkept') return true
  return loginIsCurrent(me.user.id, me.session)
}

export const decisionOf = (p: PendingSignOut): Decision => ({
  keep: p.keep,
  approved: new Map(p.approved),
  discardUnreadable: p.discardUnreadable ?? false,
})
