// @vitest-environment jsdom

import { act } from '@testing-library/react'
import { afterAll, beforeAll, expect, test, vi } from 'vitest'
import { toast } from './Toast'

beforeAll(() => {
  vi.useFakeTimers()
})
afterAll(() => {
  const left = vi.getTimerCount()
  vi.useRealTimers()
  expect(left).toBe(0)
})

test('the last toast of a file leaves no timer behind it', () => {
  act(() => {
    toast('Saved')
  })
  expect(vi.getTimerCount()).toBeGreaterThan(0)
})
