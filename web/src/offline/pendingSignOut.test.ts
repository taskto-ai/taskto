// @vitest-environment jsdom

// The record of a sign-out not finished yet: only a whole, well-formed record is acted on, and one
// account's never stands in another's way.

import { beforeEach, expect, test } from 'vitest'
import {
  backFromSignOut,
  confirmSignOut,
  pendingSignOutOf,
  pendingSignOuts,
  recordSignOut,
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
      // Cut short, or written by something else.
      { user: 1 },
      { user: 1, session: 's1', phase: 'gone', keep: false, approved: [] },
      { user: 1, session: 's1', phase: 'requested', keep: 'no', approved: [] },
      { user: 1, session: 's1', phase: 'requested', keep: false, approved: [['r1', { version: 'x' }]] },
      { user: 0, session: '', phase: 'requested', keep: false, approved: [] },
      // Whole: the second person's.
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
  // The person went back from a sign-out of their newer login (its logout got no answer).
  recordSignOut(1, 's2', false, new Map())
  unansweredSignOut(1, 's2')
  backFromSignOut(1, 's2')
  // An older login's logout, sent long before, is confirmed now.
  confirmSignOut(1, 's1')
  expect(pendingSignOutOf(1)).toMatchObject({ session: 's2', phase: 'backed' })
  confirmSignOut(1, 's2')
  expect(pendingSignOutOf(1)).toMatchObject({ session: 's2', phase: 'confirmed' })
})

test("an older login's unanswered logout and Back leave the record of a newer login of the account as it is", () => {
  // Another tab signed the account in again and is signing that newer login out.
  recordSignOut(1, 's2', false, new Map())
  // The older login's logout got no answer, and this tab goes back from it.
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
  // Signed out again from the same login: this logout is refused, and the person goes back.
  recordSignOut(1, 's1', false, new Map())
  backFromSignOut(1, 's1')
  // The first logout may still reach the server: the sign-out is kept, as gone back from.
  expect(pendingSignOutOf(1)).toMatchObject({ session: 's1', phase: 'backed', unanswered: true })
  // Not for another login of the account.
  recordSignOut(1, 's2', false, new Map())
  expect(pendingSignOutOf(1)?.unanswered).toBeUndefined()
})
