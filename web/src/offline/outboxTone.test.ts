// @vitest-environment jsdom

import { expect, test } from 'vitest'
import { toneOf } from './OutboxList'
import type { OutboxItem } from './store'

const item = (state: OutboxItem['state'], attention?: OutboxItem['attention']) => ({ state, attention }) as OutboxItem

test('each state of a queued record has its tone', () => {
  expect(toneOf(item('draft'))).toBe('neutral')
  expect(toneOf(item('queued'))).toBe('neutral')
  expect(toneOf(item('syncing'))).toBe('info')
  expect(toneOf(item('synced'))).toBe('success')
  expect(toneOf(item('attention', 'conflict'))).toBe('warning')
  expect(toneOf(item('attention', 'deleted'))).toBe('warning')
  expect(toneOf(item('attention', 'refused'))).toBe('danger')
  expect(toneOf(item('attention', 'gave_up'))).toBe('danger')
})
