// @vitest-environment jsdom

import { act, cleanup, renderHook } from '@testing-library/react'
import { afterEach, expect, test } from 'vitest'
import { useOfflineData } from './hooks'
import type { Runtime } from './runtime'
import type { OutboxItem } from './store'

afterEach(cleanup)

const item = (id: string) => ({ id }) as OutboxItem

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

  h.announce([item('a')])
  h.announce([item('a'), item('b')])
  h.announce([item('a'), item('b'), item('c')])
  expect(h.reads).toHaveLength(1)

  await act(async () => h.reads[0]?.answer())
  expect(h.reads).toHaveLength(2)
  expect(h.reads[1]?.items.map((i) => i.id)).toEqual(['a', 'b', 'c'])
  await act(async () => h.reads[1]?.answer())
  expect(result.current.items.map((i) => i.id)).toEqual(['a', 'b', 'c'])
  expect(result.current.loaded).toBe(true)
  expect(h.reads).toHaveLength(2)

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
