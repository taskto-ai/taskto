// @vitest-environment jsdom

import { afterEach, expect, test, vi } from 'vitest'
import { ApiError } from './api'
import { withOperation } from './apps/operations'
import {
  claimDrafts,
  draftKey,
  forgetDrafts,
  forgetDraftsOf,
  readDraft,
  readState,
  writeDraft,
  writeState,
} from './uiState'

afterEach(() => localStorage.clear())

test("another account signing in on this browser finds none of the last one's unsent drafts", () => {
  claimDrafts(1)
  writeState('taskto:message:/api/workspaces/1/conversations/5', 'half a thought')
  writeState('taskto:builder-start:1:private', 'an expense tracker')
  writeState('taskto:builder:1:last', '5')
  writeState('taskto:theme', 'dark')

  claimDrafts(1) // the same person again: kept
  expect(readState('taskto:builder-start:1:private')).toBe('an expense tracker')

  claimDrafts(2)
  expect(readState('taskto:message:/api/workspaces/1/conversations/5')).toBeNull()
  expect(readState('taskto:builder-start:1:private')).toBeNull()
  expect(readState('taskto:builder:1:last')).toBeNull()
  expect(readState('taskto:theme')).toBe('dark') // not a draft
})

test('signing out forgets the drafts', () => {
  claimDrafts(1)
  writeState('taskto:builder-pane:1:5', 'preview')
  forgetDrafts()
  expect(readState('taskto:builder-pane:1:5')).toBeNull()
})

// Each tab of a browser is a copy of this module: they share only the browser's storage.
async function tab() {
  vi.resetModules()
  return import('./uiState')
}

test("a tab Alice left open can't write her unsent text where Bob, signed in through another tab, reads", async () => {
  const alice = await tab()
  alice.claimDrafts(1)
  const aliceKey = alice.draftKey('message', '/api/workspaces/1/conversations/5')
  alice.writeDraft(aliceKey, 'Alice’s private plan')
  expect(alice.readDraft(aliceKey)).toBe('Alice’s private plan')

  // Alice signs out, and Bob signs in through another tab, which finds nothing of hers.
  const bob = await tab()
  bob.claimDrafts(2)
  const bobKey = bob.draftKey('message', '/api/workspaces/1/conversations/5')
  expect(bob.readDraft(bobKey)).toBeNull()

  // Alice's tab goes on typing: her text goes nowhere that Bob's tab reads, or anywhere at all.
  alice.writeDraft(aliceKey, 'Alice’s private plan, continued')
  expect(bob.readDraft(bobKey)).toBeNull()
  expect(alice.readDraft(aliceKey)).toBeNull()
  expect(Object.values({ ...localStorage }).join('|')).not.toContain('Alice')

  // Even a write that got through lands in a key of her own, never Bob's.
  expect(aliceKey).not.toBe(bobKey)
  writeState(aliceKey as string, 'stray')
  expect(bob.readDraft(bobKey)).toBeNull()

  // Bob's own drafts work, and signing out forgets them.
  bob.writeDraft(bobKey, 'Bob’s note')
  expect(bob.readDraft(bobKey)).toBe('Bob’s note')
  bob.forgetDrafts()
  expect(bob.readDraft(bobKey)).toBeNull()
  expect(readState(bobKey as string)).toBeNull()
})

test('drafts belong to the signed-in user: with nobody signed in there are none', async () => {
  const page = await tab()
  expect(page.draftKey('message', 'x')).toBeNull()
  page.writeDraft(page.draftKey('message', 'x'), 'text')
  expect(Object.keys({ ...localStorage })).toEqual([])
  page.claimDrafts(3)
  expect(page.draftKey('builder', 1, 'last')).toBe('taskto:builder:3:1:last')
  expect(page.draftKey('message', '/a')).toBe('taskto:message:3:/a')
  page.forgetDrafts()
  expect(page.draftKey('message', 'x')).toBeNull()
})

test('drafts written through the module in use are read back by it', () => {
  claimDrafts(4)
  const key = draftKey('builder-pane', 1, 9)
  writeDraft(key, 'preview')
  expect(readDraft(key)).toBe('preview')
})

test("forgetting one account's drafts leaves the account that signed in since, in another tab, its drafts and its claim", async () => {
  const alice = await tab()
  alice.claimDrafts(1)
  const aliceKey = alice.draftKey('message', 'c1')
  alice.writeDraft(aliceKey, 'Alice’s text')
  // Alice's deletion is under way; Bob signs in through another tab and writes a draft.
  const bob = await tab()
  bob.claimDrafts(2)
  const bobKey = bob.draftKey('message', 'c1')
  bob.writeDraft(bobKey, 'Bob’s text')
  writeState('taskto:builder-pane:2:5', 'Bob’s preview')

  // The answer to Alice's deletion arrives.
  alice.forgetDraftsOf(1)

  expect(readState(bobKey as string)).toBe('Bob’s text')
  expect(readState('taskto:builder-pane:2:5')).toBe('Bob’s preview')
  expect(readState('taskto:drafts-of')).toBe('2')
  expect(bob.readDraft(bobKey)).toBe('Bob’s text')
  // Hers are gone, and her tab writes nothing more.
  expect(readState(aliceKey as string)).toBeNull()
  alice.writeDraft(aliceKey, 'more')
  expect(readState(aliceKey as string)).toBeNull()
})

test("forgetting an account's drafts when nobody else has signed in removes its claim too", () => {
  claimDrafts(1)
  writeDraft(draftKey('builder', 1, 'last'), '5')
  forgetDraftsOf(1)
  expect(readState('taskto:builder:1:1:last')).toBeNull()
  expect(readState('taskto:drafts-of')).toBeNull()
  expect(draftKey('message', 'x')).toBeNull()
})

// A sign-out confirmed, or the account deleted: the app submissions it still had waiting for an
// answer go with it, in memory and in this tab's session storage, and another account's stay.
test("forgetting an account's drafts forgets its pending app submissions, and no one else's", async () => {
  const unknown = () => Promise.reject(new ApiError(502, 'bad gateway'))
  const press = { name: 'add', context: { amount: '2' } }
  claimDrafts(1)
  let first = ''
  await withOperation('app-1', press, (op) => {
    first = op
    return unknown()
  }).catch(() => {})
  expect(sessionStorage.getItem('taskto:operations:1')).toContain(first)
  sessionStorage.setItem('taskto:operations:2', JSON.stringify([['k', 'op-of-the-second']]))

  forgetDraftsOf(1)
  expect(sessionStorage.getItem('taskto:operations:1')).toBeNull()
  expect(sessionStorage.getItem('taskto:operations:2')).toContain('op-of-the-second')

  // The same person signing in again presses anew: a new submission, not the forgotten one.
  sessionStorage.removeItem('taskto:operations:2')
  claimDrafts(1)
  let again = ''
  await withOperation('app-1', press, (op) => {
    again = op
    return unknown()
  }).catch(() => {})
  expect(again).not.toBe(first)
})

// Another account claimed the browser's drafts in another tab, then this tab's person signs in here
// again: their submission still waiting for an answer keeps its key, so pressing it again can't make it
// twice.
test("a claim that changes hands keeps the claiming account's pending submissions", async () => {
  const unknown = () => Promise.reject(new ApiError(502, 'bad gateway'))
  const press = { name: 'add', context: { amount: '3' } }
  claimDrafts(1)
  let first = ''
  await withOperation('app-1', press, (op) => {
    first = op
    return unknown()
  }).catch(() => {})
  // Another tab: the second person claims the drafts.
  localStorage.setItem('taskto:drafts-of', '2')
  claimDrafts(1)
  expect(sessionStorage.getItem('taskto:operations:1')).toContain(first)
  let again = ''
  await withOperation('app-1', press, (op) => {
    again = op
    return unknown()
  }).catch(() => {})
  expect(again).toBe(first)
  forgetDrafts()
})
