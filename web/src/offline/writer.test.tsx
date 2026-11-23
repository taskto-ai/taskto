// @vitest-environment jsdom
//
// One tab at a time changes existing records offline (docs/offline.md, "One writer"): the tab that
// holds the offline writer (a Web Lock, or a lease in the database where there are none) edits,
// deletes, chooses about a conflict and discards; the other tabs of the account show the queue and
// the records and say that changes are made elsewhere, with a button that takes the right over.
// The database, not what a tab believes, decides who writes: a tab that lost the right is refused
// in the transaction it writes in.

import 'fake-indexeddb/auto'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { setPreferences } from '../i18n'
import { useOfflineData } from './hooks'
import { OutboxList } from './OutboxList'
import { deactivate, rememberUser } from './runtime'
import { NotWriter, OfflineStore, type OutboxItem, scopeKey } from './store'
import { SyncEngine } from './sync'
import { WriterNotice } from './WriterNotice'
import { OfflineWriter, WRITER_LEASE_MS, type WriterDeps } from './writer'

const scope = { workspace: 1, space: 2, app: 1 }
const base = '/api/workspaces/1/spaces/2/apps/1'

beforeEach(async () => {
  globalThis.indexedDB = new IDBFactory()
  window.localStorage.clear()
  rememberUser({ id: 7, name: 'Pat' })
  await setPreferences({})
})

afterEach(async () => {
  cleanup()
  vi.unstubAllGlobals()
  await deactivate({ purge: false })
})

// A small Web Locks: exclusive locks by name, the ones that ask to wait queued, `ifAvailable` answered
// with null when held, `signal` cancelling a wait.
class FakeLocks {
  private held = new Set<string>()
  private queue = new Map<string, (() => void)[]>()
  request(
    name: string,
    options: { ifAvailable?: boolean; signal?: AbortSignal },
    cb: (lock: unknown) => Promise<unknown>,
  ): Promise<unknown> {
    if (options.ifAvailable && this.held.has(name)) return Promise.resolve(cb(null))
    return new Promise((resolve, reject) => {
      const run = async () => {
        this.held.add(name)
        try {
          resolve(await cb({ name }))
        } catch (e) {
          reject(e)
        } finally {
          this.held.delete(name)
          this.queue.get(name)?.shift()?.()
        }
      }
      if (!this.held.has(name)) {
        void run()
        return
      }
      const waiting = this.queue.get(name) ?? []
      waiting.push(run)
      this.queue.set(name, waiting)
      options.signal?.addEventListener('abort', () => {
        this.queue.set(
          name,
          (this.queue.get(name) ?? []).filter((w) => w !== run),
        )
        reject(new DOMException('aborted', 'AbortError'))
      })
    })
  }
}

// The channel the tabs of an account hear each other on, delivered to the others.
function channels() {
  const all = new Set<{ onmessage: ((e: MessageEvent) => void) | null }>()
  return (_name: string) => {
    const mine: {
      onmessage: ((e: MessageEvent) => void) | null
      postMessage: (d: unknown) => void
      close: () => void
    } = {
      onmessage: null,
      postMessage: (data) => {
        for (const other of all) if (other !== mine) queueMicrotask(() => other.onmessage?.({ data } as MessageEvent))
      },
      close: () => void all.delete(mine),
    }
    all.add(mine)
    return mine as never
  }
}

function clock() {
  let at = 1_000_000
  return {
    now: () => at,
    advance: (ms: number) => {
      at += ms
    },
  }
}

// Two tabs of one account: two connections to one database, each with its own writer.
async function tabs(deps: (c: ReturnType<typeof clock>) => WriterDeps) {
  const c = clock()
  const channel = channels()
  const common = { now: c.now, heartbeat: () => () => {}, channel, ...deps(c) }
  const a = await OfflineStore.open(7)
  const b = await OfflineStore.open(7)
  const wa = new OfflineWriter(a, common)
  const wb = new OfflineWriter(b, common)
  await wa.start()
  await wb.start()
  return { a, b, wa, wb, c }
}

function itemOf(over: Partial<OutboxItem> = {}): Omit<OutboxItem, 'seq' | 'version'> {
  return {
    id: 'item-1',
    kind: 'insert',
    user: 7,
    scopeKey: scopeKey(scope),
    ...scope,
    base,
    release: 3,
    schema: 1,
    action: {
      name: 'add_expense',
      surfaceId: 'app-1',
      sourceComponentId: 'add',
      timestamp: '2026-11-02T10:00:00.000Z',
      context: { description: 'Tea', amount: '2' },
    },
    operation: 'op-00000001',
    record: { id: -5, revision: null },
    attachments: [],
    state: 'attention',
    attention: 'refused',
    attempts: 1,
    createdAt: '2026-11-02T10:00:00.000Z',
    updatedAt: '2026-11-02T10:00:00.000Z',
    ...over,
  }
}

const discardOf = (s: OfflineStore, item: OutboxItem) =>
  s.discardItem(item.id, { version: item.version, operation: item.operation, state: item.state })

for (const withLocks of [true, false]) {
  const how = withLocks ? 'with Web Locks' : 'with a lease in the database'
  const mode = (c: ReturnType<typeof clock>): WriterDeps => ({
    locks: withLocks ? (new FakeLocks() as never) : null,
    setTimer: (fn) => setTimeout(fn, 0),
    now: c.now,
  })

  test(`${how}: the first tab is the writer, the other reads, and taking over makes it the writer and the first a reader`, async () => {
    const shared = new FakeLocks()
    const { a, b, wa, wb } = await tabs((c) => ({ ...mode(c), locks: withLocks ? (shared as never) : null }))
    expect(wa.holds()).toBe(true)
    expect(wb.holds()).toBe(false)
    const saved = await a.enqueue(itemOf(), [])
    // The reader changes nothing of the existing queue, whatever its screen says.
    await expect(discardOf(b, saved)).rejects.toBeInstanceOf(NotWriter)
    expect(await b.listItems()).toHaveLength(1)

    await wb.takeOver()
    expect(wb.holds()).toBe(true)
    await waitFor(() => expect(wa.holds()).toBe(false))
    // The one that let go is refused by the database, and the new one writes.
    await expect(discardOf(a, saved)).rejects.toBeInstanceOf(NotWriter)
    await discardOf(b, saved)
    expect(await a.listItems()).toEqual([])
  })

  test(`${how}: a tab that closes lets go, and the next takes the right at once`, async () => {
    const shared = new FakeLocks()
    const { a, wa, wb } = await tabs((c) => ({ ...mode(c), locks: withLocks ? (shared as never) : null }))
    wa.stop()
    await waitFor(async () => expect(await a.writerIs(wa.owner)).toBe(false))
    await wb.takeOver()
    expect(wb.holds()).toBe(true)
  })
}

test('without Web Locks a holder that vanished loses the lease when it runs out, and is refused by the database even if it never heard', async () => {
  // No channel: the holder is never asked to let go, and its heartbeat has stopped (the tab froze).
  const c = clock()
  const a = await OfflineStore.open(7)
  const b = await OfflineStore.open(7)
  const timers: (() => void)[] = []
  const deps = (): WriterDeps => ({
    locks: null,
    now: c.now,
    heartbeat: () => () => {},
    channel: () => null,
    setTimer: (fn) => timers.push(fn),
  })
  const wa = new OfflineWriter(a, deps())
  const wb = new OfflineWriter(b, deps())
  await wa.start()
  await wb.start()
  expect([wa.holds(), wb.holds()]).toEqual([true, false])
  const saved = await a.enqueue(itemOf(), [])
  let took = false
  const waiting = wb.takeOver().then(() => {
    took = true
  })
  await waitFor(() => expect(timers.length).toBe(1))
  // Not yet: the lease is the holder's until it runs out.
  timers.shift()?.()
  await waitFor(() => expect(timers.length).toBe(1))
  expect(took).toBe(false)
  c.advance(WRITER_LEASE_MS + 1)
  timers.shift()?.()
  await waiting
  expect(wb.holds()).toBe(true)
  // The first tab still believes it holds the right: the database says it doesn't.
  expect(wa.holds()).toBe(true)
  await expect(discardOf(a, saved)).rejects.toBeInstanceOf(NotWriter)
  await discardOf(b, saved)
})

function Harness({ runtime }: { runtime: Parameters<typeof OutboxList>[0]['runtime'] }) {
  const data = useOfflineData(runtime)
  return (
    <>
      <WriterNotice runtime={runtime} />
      <OutboxList
        runtime={runtime}
        items={data.items}
        status={data.status}
        apps={{ [scopeKey(scope)]: 'Expenses' }}
        downloads={data.downloads}
      />
    </>
  )
}

test('a tab that is not the writer shows the queue read-only, says so, and its button takes the right over', async () => {
  const c = clock()
  const channel = channels()
  const locks = new FakeLocks()
  const deps: WriterDeps = { locks: locks as never, now: c.now, channel, heartbeat: () => () => {} }
  const a = await OfflineStore.open(7)
  const b = await OfflineStore.open(7)
  const wa = new OfflineWriter(a, deps)
  const wb = new OfflineWriter(b, deps)
  await wa.start()
  await wb.start()
  await a.enqueue(itemOf(), [])
  const runtimeOf = (store: OfflineStore, writer: OfflineWriter) => ({
    user: 7,
    store,
    engine: new SyncEngine(store, { now: c.now, setTimer: () => 0, clearTimer: () => {} }),
    writer,
    stop: () => {},
  })
  const reader = runtimeOf(b, wb)
  render(<Harness runtime={reader} />)
  // The item is there, and nothing in this tab changes it.
  await screen.findByText(/Expenses: Tea/)
  expect(screen.getByText(/Changes are being made in another tab/)).toBeTruthy()
  expect(screen.queryByRole('button', { name: /Discard/ })).toBeNull()
  expect(screen.queryByRole('button', { name: /Review/ })).toBeNull()
  expect(screen.queryByRole('button', { name: /Try again/ })).toBeNull()

  fireEvent.click(screen.getByRole('button', { name: 'Make changes here' }))
  // While it waits it says so, and once it holds the right the notice is gone.
  await waitFor(() => expect(wb.holds()).toBe(true))
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Make changes here' })).toBeNull())
  expect(screen.queryByText(/Changes are being made in another tab/)).toBeNull()
  expect(await screen.findByRole('button', { name: /Discard/ })).toBeTruthy()
  expect(wa.holds()).toBe(false)
})
