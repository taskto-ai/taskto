import { expect, test } from 'vitest'
import { type Entry, mergeView, prependPage } from './history'

const entries = (...ids: number[]): Entry[] => ids.map((id) => ({ id, kind: 'pi.user' }))
const ids = (h: { entries: Entry[] }) => h.entries.map((e) => e.id)

test('the first window is the history', () => {
  expect(mergeView(null, { entries: entries(5, 6), before: 4 })).toEqual({ entries: entries(5, 6), before: 4 })
  expect(mergeView(null, {})).toEqual({ entries: [], before: undefined })
})

test('entries that leave the window stay', () => {
  const first = mergeView(null, { entries: entries(5, 6, 7), before: 4 })
  // The window drops its oldest: they stay, and so does what came before them.
  const moved = mergeView(first, { entries: entries(7, 8), before: 6 })
  expect(ids(moved)).toEqual([5, 6, 7, 8])
  expect(moved.before).toBe(4)
})

test('loaded pages go before the window, and stay as it moves', () => {
  let h = mergeView(null, { entries: entries(5, 6), before: 4 })
  h = prependPage(h, { entries: entries(3, 4), before: 2 }, 5)
  expect(ids(h)).toEqual([3, 4, 5, 6])
  expect(h.before).toBe(2)
  h = mergeView(h, { entries: entries(5, 6, 7), before: 4 })
  expect(ids(h)).toEqual([3, 4, 5, 6, 7])
  h = prependPage(h, { entries: entries(1, 2) }, 3)
  expect(ids(h)).toEqual([1, 2, 3, 4, 5, 6, 7])
  expect(h.before).toBeUndefined()
})

test('a page for where the history no longer starts is dropped', () => {
  const h = mergeView(null, { entries: entries(5, 6), before: 4 })
  expect(prependPage(h, { entries: entries(1, 2) }, 9)).toBe(h)
})

test('a window that no longer joins what was kept starts again', () => {
  const h = mergeView(null, { entries: entries(5, 6), before: 4 })
  // A reconnect that missed 7 to 9: what was kept can't be shown with a hole in it.
  expect(mergeView(h, { entries: entries(10, 11), before: 9 })).toEqual({ entries: entries(10, 11), before: 9 })
})
