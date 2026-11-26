// @vitest-environment jsdom
//
// What a page reads of the queue is read one read at a time: a burst of announcements from the
// other tabs costs at most one more read, and an older read never lands after a newer one
// (docs/offline.md, "Versions").

import { act, cleanup, renderHook } from '@testing-library/react'
import { afterEach, expect, test } from 'vitest'
import { useOfflineData } from './hooks'
import type { Runtime } from './runtime'
import type { OutboxItem } from './store'

afterEach(cleanup)

const item = (id: string) => ({ id }) as OutboxItem

// A runtime whose reads of the queue are answered when the test says so, each with the queue as it
// was when the read began.
function held() {
  let queue: OutboxItem[] = []
  const reads: { items: OutboxItem[]; answer: () => void }[] = []
  const listeners = new Set<() => void>()
  const runtime = {
    store: {
      subscribe: (fn: () => void) => {
        listeners.add(fn)
        return () => listeners.delete(fn)
      },
      listDownloads: async () => [],
      listItems: () =>
        new Promise<OutboxItem[]>((resolve) => {
          const items = queue
          reads.push({ items, answer: () => resolve(items) })
        }),
    },
    engine: { subscribe: () => () => {}, status: () => ({ paused: null, running: false, online: true }) },
    writer: { subscribe: () => () => {} },
  } as unknown as Runtime
  return {
    runtime,
    reads,
    listeners: () => listeners.size,
    // Another tab changed the queue and said so.
    announce: (next?: OutboxItem[]) => {
      if (next) queue = next
      for (const fn of [...listeners]) fn()
    },
  }
}

test('reads are one at a time: a burst of announcements costs one more read, and the newest is what is shown', async () => {
  const h = held()
  const { result } = renderHook(() => useOfflineData(h.runtime))
  expect(h.reads).toHaveLength(1)

  // While the first read is on its way the queue changes three times.
  h.announce([item('a')])
  h.announce([item('a'), item('b')])
  h.announce([item('a'), item('b'), item('c')])
  // No second read runs beside the first: one that began earlier could finish after a later one.
  expect(h.reads).toHaveLength(1)

  await act(async () => h.reads[0]?.answer())
  // The first read's answer is shown, and one read more follows, of the queue as it is now.
  expect(h.reads).toHaveLength(2)
  expect(h.reads[1]?.items.map((i) => i.id)).toEqual(['a', 'b', 'c'])
  await act(async () => h.reads[1]?.answer())
  expect(result.current.items.map((i) => i.id)).toEqual(['a', 'b', 'c'])
  expect(result.current.loaded).toBe(true)
  // Nothing else was read for the three announcements.
  expect(h.reads).toHaveLength(2)

  // Quiet again: the next announcement is read at once.
  h.announce([])
  expect(h.reads).toHaveLength(3)
  await act(async () => h.reads[2]?.answer())
  expect(result.current.items).toEqual([])
})

test('a read that finishes after the page is gone shows nothing, and no more are started', async () => {
  const h = held()
  const { result, unmount } = renderHook(() => useOfflineData(h.runtime))
  h.announce([item('a')])
  unmount()
  expect(h.listeners()).toBe(0)
  await act(async () => h.reads[0]?.answer())
  expect(h.reads).toHaveLength(1)
  expect(result.current.loaded).toBe(false)
})
