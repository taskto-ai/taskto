// @vitest-environment jsdom

import { beforeEach, expect, test, vi } from 'vitest'
import {
  backFromSignOut,
  beginLogout,
  browserLogin,
  confirmSignOut,
  endLogout,
  ensureSignOut,
  forgetLogin,
  pendingSignOutOf,
  pendingSignOuts,
  recordSignOut,
  rememberLogin,
  settleSignOut,
  signOutPending,
  unansweredSignOut,
} from './pendingSignOut'

const KEY = 'taskto:sign-out-pending'

beforeEach(() => localStorage.clear())

test('a sign-out is recorded with its login and phase, confirmed, and settled', () => {
  recordSignOut(1, 's1', false, new Map([['r1', { version: 2, operation: 'op-1' }]]))
  expect(pendingSignOutOf(1)).toEqual({
    user: 1,
    session: 's1',
    phase: 'requested',
    keep: false,
    approved: [['r1', { version: 2, operation: 'op-1' }]],
    at: expect.any(Number),
  })
  confirmSignOut(1, 's1')
  expect(pendingSignOutOf(1)?.phase).toBe('confirmed')
  settleSignOut(1)
  expect(signOutPending(1)).toBe(false)
  expect(localStorage.getItem(KEY)).toBeNull()
})

test('a record that is not a whole one is not acted on, and another account’s does not stand in the way', () => {
  localStorage.setItem(
    KEY,
    JSON.stringify([
      { user: 1 },
      { user: 1, session: 's1', phase: 'gone', keep: false, approved: [] },
      { user: 1, session: 's1', phase: 'requested', keep: 'no', approved: [] },
      { user: 1, session: 's1', phase: 'requested', keep: false, approved: [['r1', { version: 'x' }]] },
      { user: 0, session: '', phase: 'requested', keep: false, approved: [] },
      { user: 2, session: 's2', phase: 'confirmed', keep: true, approved: [] },
    ]),
  )
  expect(pendingSignOuts().map((p) => p.user)).toEqual([2])
  expect(signOutPending(1)).toBe(false)
  expect(signOutPending(2)).toBe(true)
})

test('storage that is not a list is no record at all', () => {
  localStorage.setItem(KEY, '{not json')
  expect(pendingSignOuts()).toEqual([])
  localStorage.setItem(KEY, JSON.stringify({ user: 1 }))
  expect(pendingSignOuts()).toEqual([])
})

test("a confirmation of one login's sign-out leaves the record of a newer login of the account as it is", () => {
  recordSignOut(1, 's2', false, new Map())
  unansweredSignOut(1, 's2')
  backFromSignOut(1, 's2')
  confirmSignOut(1, 's1')
  expect(pendingSignOutOf(1)).toMatchObject({ session: 's2', phase: 'backed' })
  confirmSignOut(1, 's2')
  expect(pendingSignOutOf(1)).toMatchObject({ session: 's2', phase: 'confirmed' })
})

test("an older login's unanswered logout and Back leave the record of a newer login of the account as it is", () => {
  recordSignOut(1, 's2', false, new Map())
  unansweredSignOut(1, 's1')
  backFromSignOut(1, 's1')
  expect(pendingSignOutOf(1)).toMatchObject({ session: 's2', phase: 'requested' })
  expect(pendingSignOutOf(1)?.unanswered).toBeUndefined()
  expect(signOutPending(1)).toBe(true)
})

test('a sign-out of the same login again keeps that an earlier logout of it got no answer', () => {
  recordSignOut(1, 's1', false, new Map())
  unansweredSignOut(1, 's1')
  backFromSignOut(1, 's1')
  recordSignOut(1, 's1', false, new Map())
  backFromSignOut(1, 's1')
  expect(pendingSignOutOf(1)).toMatchObject({ session: 's1', phase: 'backed', unanswered: true })
  recordSignOut(1, 's2', false, new Map())
  expect(pendingSignOuts().find((p) => p.session === 's2')?.unanswered).toBeUndefined()
  expect(pendingSignOuts().find((p) => p.session === 's1')).toMatchObject({ phase: 'backed', unanswered: true })
})

test('the browser keeps the login it learned last of an account, whatever order the answers come in', () => {
  rememberLogin(1, 's2', 200)
  rememberLogin(1, 's1', 100)
  expect(browserLogin()).toMatchObject({ user: 1, session: 's2' })
  rememberLogin(2, 't1', 300)
  expect(browserLogin()).toMatchObject({ user: 2, session: 't1' })
  forgetLogin(2, 't0')
  expect(browserLogin()).toMatchObject({ user: 2, session: 't1' })
  forgetLogin(2, 't1')
  expect(browserLogin()).toBeNull()
})

test("two tabs recording sign-outs of two logins at once keep both: neither writes back the other's from what it read", () => {
  const set = Storage.prototype.setItem
  let raced = false
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation(function (this: Storage, key: string, value: string) {
    if (!raced && key.startsWith(KEY)) {
      raced = true
      recordSignOut(1, 's2', false, new Map([['r2', { version: 1, operation: 'op-2' }]]))
    }
    set.call(this, key, value)
  })
  recordSignOut(1, 's1', false, new Map())
  vi.restoreAllMocks()
  expect(
    pendingSignOuts()
      .map((p) => p.session)
      .sort(),
  ).toEqual(['s1', 's2'])
  expect(pendingSignOuts().find((p) => p.session === 's2')?.approved).toEqual([
    ['r2', { version: 1, operation: 'op-2' }],
  ])
})

test('a logout with no answer keeps its mark when the browser refuses to note that on the sign-out', () => {
  recordSignOut(1, 's1', false, new Map())
  const logout = beginLogout(1, 's1')
  const set = Storage.prototype.setItem
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation(function (this: Storage, key: string, value: string) {
    if (key.startsWith(`${KEY}:`)) throw new DOMException('full', 'QuotaExceededError')
    set.call(this, key, value)
  })
  endLogout(logout, 1, 's1', false)
  vi.restoreAllMocks()
  backFromSignOut(1, 's1')
  expect(pendingSignOuts().find((p) => p.session === 's1')).toMatchObject({ phase: 'backed' })
})

test('a sign-out gone back from is pending again when the person tries to sign out again, its earlier logout still kept', () => {
  recordSignOut(1, 's1', false, new Map())
  unansweredSignOut(1, 's1')
  backFromSignOut(1, 's1')
  expect(signOutPending(1)).toBe(false)
  ensureSignOut(1, 's1', { keep: false, approved: new Map() })
  expect(signOutPending(1)).toBe(true)
  expect(pendingSignOuts().find((p) => p.session === 's1')).toMatchObject({ phase: 'requested', unanswered: true })
})

test('a sign-out put back from a decision with no time (an earlier version kept none) gets none', () => {
  ensureSignOut(1, 's1', { keep: false, approved: new Map() }, 'confirmed')
  expect(pendingSignOuts().find((p) => p.session === 's1')?.at).toBeUndefined()
})

test("recording one login's sign-out keeps another login's record of the same account", () => {
  recordSignOut(1, 's2', false, new Map([['r2', { version: 1, operation: 'op-2' }]]))
  recordSignOut(1, 's1', false, new Map())
  expect(
    pendingSignOuts()
      .map((p) => p.session)
      .sort(),
  ).toEqual(['s1', 's2'])
})
