// @vitest-environment jsdom
//
// Two tabs of one account on one database: what one tab does to the queue is shown by the other
// without a reload, and a discard made on a screen that is out of date never sends the record
// (docs/offline.md, "One writer" and "Versions").

import 'fake-indexeddb/auto'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { setPreferences } from '../i18n'
import { fakeFetch } from '../test/fakes'
import { useOfflineData } from './hooks'
import { OutboxList } from './OutboxList'
import { deactivate, type Runtime, rememberUser } from './runtime'
import { OfflineStore, type OutboxItem, scopeKey } from './store'
import { SyncEngine } from './sync'
import { WriterNotice } from './WriterNotice'
import { OfflineWriter, type WriterDeps } from './writer'

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

const at = 1_000_000
const now = () => at

// Two tabs: two connections to one database, each with its own writer (the first one holds the
// right, by the lease in the database) and its own engine.
async function twoTabs({ announce }: { announce: boolean }) {
  const writerChannel = channels()
  const changes = channels()
  const deps: WriterDeps = {
    locks: null,
    now,
    heartbeat: () => () => {},
    channel: writerChannel,
    setTimer: (fn) => setTimeout(fn, 0),
  }
  const tab = async () => {
    const store = await OfflineStore.open(7)
    if (announce) store.connectTabs(changes)
    const writer = new OfflineWriter(store, deps)
    await writer.start()
    const engine = new SyncEngine(store, { now, setTimer: () => 0, clearTimer: () => {}, random: () => 0.5 })
    const runtime = { user: 7, store, engine, writer, stop: () => {} } as unknown as Runtime
    return { store, writer, runtime }
  }
  const a = await tab()
  const b = await tab()
  return { a, b }
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
    state: 'draft',
    attempts: 0,
    createdAt: '2026-11-02T10:00:00.000Z',
    updatedAt: '2026-11-02T10:00:00.000Z',
    ...over,
  }
}

function Harness({ runtime }: { runtime: Runtime }) {
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

const DRAFT = 'Draft on this device'
const QUEUED = 'Saved on this device, waiting to sync'

test('a record another tab queued is in this tab’s list without a reload', async () => {
  const { a, b } = await twoTabs({ announce: true })
  render(<Harness runtime={a.runtime} />)
  await screen.findByText('Nothing is waiting to be sent.')
  expect(screen.queryByText(/Expenses: Tea/)).toBeNull()

  await b.store.enqueue(itemOf(), [])
  await screen.findByText(/Expenses: Tea/)
  expect(screen.getByText(DRAFT)).toBeTruthy()

  // A change of that tab to the item is shown the same way.
  await b.store.updateItem('item-1', (i) => ({ ...i, state: 'queued' }))
  await screen.findByText(QUEUED)
  expect(screen.queryByText(DRAFT)).toBeNull()
})

test('a tab that takes the writer role back shows the queue as the tab that held it left it', async () => {
  // No channel between the tabs' stores: only the role coming back tells this tab to read again.
  const { a, b } = await twoTabs({ announce: false })
  await a.store.enqueue(itemOf(), [])
  render(<Harness runtime={a.runtime} />)
  await screen.findByText(DRAFT)

  // The other tab takes over, sends the draft back to the queue, and is closed.
  await b.writer.takeOver()
  await waitFor(() => expect(a.writer.holds()).toBe(false))
  await b.store.updateItem('item-1', (i) => ({ ...i, state: 'queued' }), { guarded: true })
  b.writer.stop()

  fireEvent.click(await screen.findByRole('button', { name: 'Make changes here' }))
  await waitFor(() => expect(a.writer.holds()).toBe(true))
  await screen.findByText(QUEUED)
  expect(screen.queryByText(DRAFT)).toBeNull()
})

test('a discard refused because the list was stale reads the queue again, and the next one acts on what is there and sends nothing', async () => {
  // Nothing tells this tab of the other's change: only the refusal makes it read again.
  const { a, b } = await twoTabs({ announce: false })
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  await a.store.enqueue(itemOf(), [])
  render(<Harness runtime={a.runtime} />)
  await screen.findByText(DRAFT)

  // Another tab released the draft meanwhile: the item is queued, and a newer version.
  await b.store.updateItem('item-1', (i) => ({ ...i, state: 'queued' }))

  fireEvent.click(screen.getByRole('button', { name: /Discard/ }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/changed in another tab/)
  expect(await a.store.listItems()).toHaveLength(1)
  // The list was read again at once: it shows what is stored, not the draft.
  await screen.findByText(QUEUED)
  expect(screen.queryByText(DRAFT)).toBeNull()

  // The same question, on what is really there: the record goes, and is never sent.
  fireEvent.click(screen.getByRole('button', { name: /Discard/ }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  await waitFor(async () => expect(await a.store.listItems()).toEqual([]))
  await a.runtime.engine.idle()
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)
})

test('a discard of a record that has gone out meanwhile is refused and says it is on its way', async () => {
  const { a, b } = await twoTabs({ announce: false })
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  await a.store.enqueue(itemOf({ state: 'queued' }), [])
  render(<Harness runtime={a.runtime} />)
  await screen.findByText(QUEUED)

  // A send of the other tab claimed it: it is syncing now.
  await b.store.updateItem('item-1', (i) => ({ ...i, state: 'syncing' }), { bump: false })

  fireEvent.click(screen.getByRole('button', { name: /Discard/ }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  const alert = await screen.findByRole('alert')
  expect(within(alert).queryByText(/on its way to the server/)).toBeTruthy()
  // Nothing was removed, and the list says where the record stands, with no way to discard it.
  expect(await a.store.listItems()).toHaveLength(1)
  await screen.findByText('Syncing…')
  expect(screen.queryByRole('button', { name: /Discard/ })).toBeNull()
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)
})
