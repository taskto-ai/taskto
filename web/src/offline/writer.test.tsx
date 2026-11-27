// @vitest-environment jsdom

import 'fake-indexeddb/auto'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import type { OfflineApp } from '../api'
import { setPreferences } from '../i18n'
import { fakeFetch } from '../test/fakes'
import { useOfflineData } from './hooks'
import { LogoutGuard } from './LogoutGuard'
import { OutboxList } from './OutboxList'
import { deactivate, type Runtime, rememberUser } from './runtime'
import { NotWriter, OfflineStore, type OutboxItem, scopeKey } from './store'
import { SyncEngine } from './sync'
import { offlineTransport } from './transport'
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
    await expect(discardOf(b, saved)).rejects.toBeInstanceOf(NotWriter)
    expect(await b.listItems()).toHaveLength(1)

    await wb.takeOver()
    expect(wb.holds()).toBe(true)
    await waitFor(() => expect(wa.holds()).toBe(false))
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
  timers.shift()?.()
  await waitFor(() => expect(timers.length).toBe(1))
  expect(took).toBe(false)
  c.advance(WRITER_LEASE_MS + 1)
  timers.shift()?.()
  await waiting
  expect(wb.holds()).toBe(true)
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
  await screen.findByText(/Expenses: Tea/)
  expect(screen.getByText(/Changes are being made in another tab/)).toBeTruthy()
  expect(screen.queryByRole('button', { name: /Discard/ })).toBeNull()
  expect(screen.queryByRole('button', { name: /Review/ })).toBeNull()
  expect(screen.queryByRole('button', { name: /Try again/ })).toBeNull()

  fireEvent.click(screen.getByRole('button', { name: 'Make changes here' }))
  await waitFor(() => expect(wb.holds()).toBe(true))
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Make changes here' })).toBeNull())
  expect(screen.queryByText(/Changes are being made in another tab/)).toBeNull()
  expect(await screen.findByRole('button', { name: /Discard/ })).toBeTruthy()
  expect(wa.holds()).toBe(false)
})

async function mixed(tell = true) {
  const c = clock()
  const channel = tell ? channels() : () => null
  const common = { now: c.now, heartbeat: () => () => {}, channel, setTimer: (fn: () => void) => setTimeout(fn, 0) }
  const a = await OfflineStore.open(7)
  const b = await OfflineStore.open(7)
  const lease = new OfflineWriter(a, { ...common, locks: null })
  const locked = new OfflineWriter(b, { ...common, locks: new FakeLocks() as never })
  return { a, b, c, lease, locked }
}

test('a tab with Web Locks does not take the right from a tab that holds it by the lease: the row in the database decides', async () => {
  const { a, lease, locked } = await mixed()
  await lease.start()
  await locked.start()
  expect([lease.holds(), locked.holds()]).toEqual([true, false])
  expect(await a.writerIs(lease.owner)).toBe(true)
  expect(await a.writerIs(locked.owner)).toBe(false)
  await locked.takeOver()
  expect(locked.holds()).toBe(true)
  await waitFor(() => expect(lease.holds()).toBe(false))
  expect(await a.writerIs(locked.owner)).toBe(true)
})

test('a tab with the lease does not take the right from a tab that holds a Web Lock, until its lease has run out', async () => {
  const { a, b, c, lease, locked } = await mixed(false)
  await locked.start()
  await lease.start()
  expect([locked.holds(), lease.holds()]).toEqual([true, false])
  expect(await a.writerIs(locked.owner)).toBe(true)
  c.advance(WRITER_LEASE_MS + 1)
  await lease.takeOver()
  expect(lease.holds()).toBe(true)
  expect(await a.writerIs(lease.owner)).toBe(true)
  const saved = await a.enqueue(itemOf(), [])
  await expect(discardOf(b, saved)).rejects.toBeInstanceOf(NotWriter)
  await discardOf(a, saved)
  expect(await a.listItems()).toEqual([])
})

test('stopping while the lock is being taken lets the lock go: no Web Lock is left held by a tab that stopped', async () => {
  const locks = new FakeLocks()
  const store = await OfflineStore.open(7)
  let open: () => void = () => {}
  const gate = new Promise<void>((resolve) => {
    open = resolve
  })
  const claim = store.claimWriter.bind(store)
  const slow = vi.spyOn(store, 'claimWriter').mockImplementation(async (...args) => {
    await gate
    return claim(...args)
  })
  const writer = new OfflineWriter(store, { locks: locks as never, now: clock().now, heartbeat: () => () => {} })
  const starting = writer.start()
  await waitFor(() => expect(slow).toHaveBeenCalled())
  writer.stop()
  open()
  await starting
  expect(writer.holds()).toBe(false)
  expect(await store.writerIs(writer.owner)).toBe(false)
  let free = false
  await locks.request(`taskto-offline-writer-7`, { ifAvailable: true }, async (lock) => {
    free = lock !== null
  })
  expect(free).toBe(true)
})

const definition: OfflineApp['definition'] = {
  definition_version: 1,
  requires: [],
  collection: {
    fields: [
      { name: 'description', type: 'string', required: true, max_length: 200 },
      { name: 'amount', type: 'number', required: true },
    ],
  },
  actions: {
    add_expense: { kind: 'insert' },
    edit_expense: { kind: 'update' },
  },
  summaries: { total: { kind: 'sum', field: 'amount' } },
}

const press = (context: Record<string, unknown>) => ({
  name: 'edit_expense',
  surfaceId: 'app-1',
  sourceComponentId: 'edit',
  timestamp: '2026-11-02T10:00:00.000Z',
  context,
  expectedRevision: 1,
})

test('a screen drawn before a hand-over cannot queue a change over the previous writer’s pending edit: it reads again first', async () => {
  const c = clock()
  const deps: WriterDeps = {
    locks: new FakeLocks() as never,
    now: c.now,
    channel: channels(),
    heartbeat: () => () => {},
  }
  const a = await OfflineStore.open(7)
  const b = await OfflineStore.open(7)
  const wa = new OfflineWriter(a, deps)
  const wb = new OfflineWriter(b, deps)
  await wa.start()
  await wb.start()
  const copy = {
    ...scope,
    key: scopeKey(scope),
    user: 7,
    name: 'Expenses',
    release: 3,
    definition,
    messages: surface,
    scopeOf: 'all' as const,
    records: 1,
    total: 1,
    truncated: false,
    canInsert: true,
    capturedAt: '2026-11-02T10:00:00.000Z',
    operations: {},
    modifiable: [1],
    bytes: 100,
  }
  await a.putDownload(copy)
  const runtime = (store: OfflineStore, writer: OfflineWriter) =>
    ({ user: 7, store, writer, engine: { run: async () => {} }, stop: () => {} }) as unknown as Runtime
  const first = offlineTransport(runtime(a, wa), copy)
  const second = offlineTransport(runtime(b, wb), copy)
  await second.load()
  await first.load()
  await first.act(press({ id: 1, amount: '9' }))
  const [mine] = await a.listItems()
  await wb.takeOver()
  expect(wb.holds()).toBe(true)
  await expect(second.act(press({ id: 1, description: 'Stale' }))).rejects.toMatchObject({
    code: 'offline_record_changed',
  })
  expect(await b.listItems()).toEqual([mine])
  await second.load()
  await second.act(press({ id: 1, description: 'Later' }))
  const [merged] = await b.listItems()
  expect(merged).toMatchObject({ id: mine.id, version: mine.version + 1 })
  expect(merged.action.context).toEqual({ id: 1, amount: '9', description: 'Later' })
})

test('a writer whose lease has run out is refused by the database, even before another tab replaces the row', async () => {
  const c = clock()
  const a = await OfflineStore.open(7)
  const wa = new OfflineWriter(a, { locks: null, now: c.now, heartbeat: () => () => {}, channel: () => null })
  await wa.start()
  expect(wa.holds()).toBe(true)
  const saved = await a.enqueue(itemOf(), [])
  c.advance(WRITER_LEASE_MS + 1)
  await expect(discardOf(a, saved)).rejects.toBeInstanceOf(NotWriter)
  expect(await a.listItems()).toHaveLength(1)
  expect(await a.claimWriter(wa.owner, c.now() + WRITER_LEASE_MS, () => false)).toBe(true)
  await discardOf(a, saved)
  expect(await a.listItems()).toEqual([])
})

async function signingOut(takeOver: 'real' | 'refused') {
  const c = clock()
  const { a, b, wb } = await tabs((cl) => ({
    locks: null,
    setTimer: (fn) => setTimeout(fn, 0),
    now: cl.now,
  }))
  await a.enqueue(itemOf(), [])
  const engine = new SyncEngine(b, { now: c.now, setTimer: () => 0, clearTimer: () => {}, random: () => 0.5 })
  const writer =
    takeOver === 'real' ? wb : ({ holds: () => wb.holds(), takeOver: async () => {} } as unknown as OfflineWriter)
  const runtime = { user: 7, store: b, engine, writer, stop: () => {} } as unknown as Runtime
  const out = vi.fn()
  render(<LogoutGuard runtime={runtime} count={1} onSignOut={out} onCancel={() => {}} />)
  return { a, b, out }
}

test('"Sync now" in a tab that is not the writer takes the right first, then syncs, and signs out', async () => {
  const { b, out } = await signingOut('real')
  fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  fireEvent.click(screen.getByRole('button', { name: 'Sync now' }))
  await waitFor(() => expect(out).toHaveBeenCalledOnce())
  expect((await b.listItems())[0].state).toBe('synced')
})

test('"Sync now" in a tab that could not take the right says so in words, and attempts nothing', async () => {
  const { b, out } = await signingOut('refused')
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  fireEvent.click(screen.getByRole('button', { name: 'Sync now' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/another tab/)
  expect(out).not.toHaveBeenCalled()
  expect(calls).toHaveLength(0)
  expect((await b.listItems())[0].state).toBe('attention')
  expect(screen.getByRole('button', { name: 'Sync now' })).toHaveProperty('disabled', false)
})
