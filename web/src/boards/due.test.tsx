// @vitest-environment jsdom

import { act, cleanup, renderHook } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { useToday } from './due'

afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

test("today moves on at the reader's midnight, and when the page is seen again", async () => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date(2026, 2, 1, 23, 59, 30))
  const { result } = renderHook(() => useToday())
  expect(result.current).toBe('2026-03-01')
  await act(() => vi.advanceTimersByTimeAsync(20_000))
  expect(result.current).toBe('2026-03-01')
  await act(() => vi.advanceTimersByTimeAsync(20_000))
  expect(result.current).toBe('2026-03-02')

  // A sleeping page's timer may not have fired: becoming visible looks at the clock again.
  vi.setSystemTime(new Date(2026, 2, 5, 9, 0, 0))
  await act(async () => {
    document.dispatchEvent(new Event('visibilitychange'))
  })
  expect(result.current).toBe('2026-03-05')
})
