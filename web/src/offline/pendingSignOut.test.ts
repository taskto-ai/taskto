// @vitest-environment jsdom

// The record of a sign-out not finished yet: only a whole, well-formed record is acted on, and one
// account's never stands in another's way.

import { beforeEach, expect, test } from 'vitest'
import {
  confirmSignOut,
  pendingSignOutOf,
  pendingSignOuts,
  recordSignOut,
  settleSignOut,
  signOutPending,
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
  confirmSignOut(1)
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
