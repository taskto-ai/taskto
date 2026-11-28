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
  writeState('taskto:note:/api/workspaces/1/conversations/5', 'a private remark')
  writeState('taskto:builder-start:1:private', 'an expense tracker')
  writeState('taskto:builder:1:last', '5')
  writeState('taskto:theme', 'dark')

  claimDrafts(1)
  expect(readState('taskto:builder-start:1:private')).toBe('an expense tracker')

  claimDrafts(2)
  expect(readState('taskto:message:/api/workspaces/1/conversations/5')).toBeNull()
  expect(readState('taskto:note:/api/workspaces/1/conversations/5')).toBeNull()
  expect(readState('taskto:builder-start:1:private')).toBeNull()
  expect(readState('taskto:builder:1:last')).toBeNull()
  expect(readState('taskto:theme')).toBe('dark')
})

test('signing out forgets the drafts', () => {
  claimDrafts(1)
  writeState('taskto:builder-pane:1:5', 'preview')
  forgetDrafts()
  expect(readState('taskto:builder-pane:1:5')).toBeNull()
})

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

  const bob = await tab()
  bob.claimDrafts(2)
  const bobKey = bob.draftKey('message', '/api/workspaces/1/conversations/5')
  expect(bob.readDraft(bobKey)).toBeNull()

  alice.writeDraft(aliceKey, 'Alice’s private plan, continued')
  expect(bob.readDraft(bobKey)).toBeNull()
  expect(alice.readDraft(aliceKey)).toBeNull()
  expect(Object.values({ ...localStorage }).join('|')).not.toContain('Alice')

  expect(aliceKey).not.toBe(bobKey)
  writeState(aliceKey as string, 'stray')
  expect(bob.readDraft(bobKey)).toBeNull()

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
  const bob = await tab()
  bob.claimDrafts(2)
  const bobKey = bob.draftKey('message', 'c1')
  bob.writeDraft(bobKey, 'Bob’s text')
  writeState('taskto:builder-pane:2:5', 'Bob’s preview')

  alice.forgetDraftsOf(1)

  expect(readState(bobKey as string)).toBe('Bob’s text')
  expect(readState('taskto:builder-pane:2:5')).toBe('Bob’s preview')
  expect(readState('taskto:drafts-of')).toBe('2')
  expect(bob.readDraft(bobKey)).toBe('Bob’s text')
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

  sessionStorage.removeItem('taskto:operations:2')
  claimDrafts(1)
  let again = ''
  await withOperation('app-1', press, (op) => {
    again = op
    return unknown()
  }).catch(() => {})
  expect(again).not.toBe(first)
})

test("a claim that changes hands keeps the claiming account's pending submissions", async () => {
  const unknown = () => Promise.reject(new ApiError(502, 'bad gateway'))
  const press = { name: 'add', context: { amount: '3' } }
  claimDrafts(1)
  let first = ''
  await withOperation('app-1', press, (op) => {
    first = op
    return unknown()
  }).catch(() => {})
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

test("forgetting an account's drafts removes its unsent notes too", () => {
  claimDrafts(1)
  const key = draftKey('note', 'c1')
  writeDraft(key, 'a private remark')
  expect(readState(key as string)).toBe('a private remark')
  forgetDraftsOf(1)
  expect(readState(key as string)).toBeNull()
})
