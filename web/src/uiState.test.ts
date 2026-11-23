// @vitest-environment jsdom

import { afterEach, expect, test } from 'vitest'
import { claimDrafts, forgetDrafts, readState, writeState } from './uiState'

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
