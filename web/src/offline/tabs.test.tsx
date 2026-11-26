// @vitest-environment jsdom
//
// Two tabs of one account on one database: what one tab does to the queue is shown by the other
// without a reload, and a discard made on a screen that is out of date never sends the record
// (docs/offline.md, "One writer" and "Versions").

import 'fake-indexeddb/auto'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { setPreferences } from '../i18n'
import { fakeFetch } from '../test/fakes'
import { holdItem, keepServer, releaseItem, retryItem } from './choices'
import { useOfflineData } from './hooks'
import { OutboxList } from './OutboxList'
import { recordSignOut, settleSignOut } from './pendingSignOut'
import { activate, deactivate, type Runtime, rememberUser, setSyncDeps } from './runtime'
import { OfflineStore, type OutboxItem, scopeKey, seenAs, seenOf } from './store'
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
  setSyncDeps({})
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

test('a discard of a record the server already has says it is on its way', async () => {
  const { a, b } = await twoTabs({ announce: false })
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  await a.store.enqueue(itemOf({ state: 'queued' }), [])
  render(<Harness runtime={a.runtime} />)
  await screen.findByText(QUEUED)

  // The other tab sent it and the server answered: this tab still shows it waiting.
  await b.store.updateItem('item-1', (i) => ({ ...i, state: 'synced' }))

  fireEvent.click(screen.getByRole('button', { name: /Discard/ }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  const alert = await screen.findByRole('alert')
  expect(within(alert).queryByText(/on its way to the server/)).toBeTruthy()
  // The acknowledged entry is never removed by a discard.
  expect(await a.store.listItems()).toHaveLength(1)
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)
})

test('a record back in the queue after a send nobody answered is not said to be on its way: the same question is asked on what is there', async () => {
  const { a, b } = await twoTabs({ announce: false })
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  await a.store.enqueue(itemOf({ state: 'queued' }), [])
  render(<Harness runtime={a.runtime} />)
  await screen.findByText(QUEUED)

  // The other tab's send got no answer: the item is back in the queue, waiting, and it may be on
  // the server (`unconfirmed`, one send more).
  await b.store.updateItem('item-1', (i) => ({ ...i, state: 'queued', unconfirmed: true, sends: (i.sends ?? 0) + 1 }))

  fireEvent.click(screen.getByRole('button', { name: /Discard/ }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  const alert = await screen.findByRole('alert')
  expect(alert.textContent).toMatch(/changed in another tab/)
  expect(alert.textContent).not.toMatch(/on its way/)
  expect(await a.store.listItems()).toHaveLength(1)
  // It is waiting, with a Discard, which is what the list shows: the next one is the person's answer
  // to a question about what is there.
  await screen.findByText(QUEUED)
  fireEvent.click(screen.getByRole('button', { name: /Discard/ }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  await waitFor(async () => expect(await a.store.listItems()).toEqual([]))
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)
})

// Named channels: the tabs that opened the same name hear one another, and a channel that was
// closed hears and says nothing.
function hub() {
  type Chan = {
    name: string
    closed: boolean
    onmessage: ((e: MessageEvent) => void) | null
    postMessage: (d: unknown) => void
    close: () => void
  }
  const all: Chan[] = []
  const open = (name: string) => {
    const chan: Chan = {
      name,
      closed: false,
      onmessage: null,
      postMessage: (data) => {
        for (const other of all)
          if (other !== chan && other.name === name && !other.closed)
            queueMicrotask(() => other.onmessage?.({ data } as MessageEvent))
      },
      close: () => {
        chan.closed = true
      },
    }
    all.push(chan)
    return chan as never
  }
  return { open, all }
}

test('the app joins the account’s channel when it starts, reads again on what it hears, and leaves it when it stops', async () => {
  const channels = hub()
  setSyncDeps(
    { now, setTimer: () => 0, clearTimer: () => {}, random: () => 0.5 },
    { locks: null, now, heartbeat: () => () => {}, channel: channels.open, setTimer: (fn) => setTimeout(fn, 0) },
  )
  fakeFetch({})
  const runtime = (await activate(7, 'Pat')).runtime as Runtime
  expect(runtime).toBeTruthy()

  // One channel for the account's changes, named for the account.
  const mine = channels.all.filter((c) => c.name.startsWith('taskto-offline-changes'))
  expect(mine.map((c) => c.name)).toEqual(['taskto-offline-changes-7'])

  render(<Harness runtime={runtime} />)
  await screen.findByText('Nothing is waiting to be sent.')

  // Another tab of the account (its own connection, on the same channel's name) queues a record:
  // this tab's list reads again, with no reload.
  const other = await OfflineStore.open(7)
  other.connectTabs(channels.open)
  await other.enqueue(itemOf(), [])
  await screen.findByText(/Expenses: Tea/)

  // Stopping lets go of the channel.
  cleanup()
  await deactivate({ purge: false })
  expect(mine[0]?.closed).toBe(true)
  other.close()
})

test('a refusal made before the store is asked also makes the lists read again', async () => {
  const { a, b } = await twoTabs({ announce: false })
  await a.store.enqueue(itemOf({ state: 'attention', attention: 'conflict' } as Partial<OutboxItem>), [])
  const shown = (await a.store.listItems())[0] as OutboxItem
  const told = vi.fn()
  a.store.subscribe(told)

  // Another tab changed the item since this one drew it: the choice is refused on the stored
  // version, before the server is asked anything.
  await b.store.updateItem('item-1', (i) => ({ ...i, state: 'attention', attempts: 1 }))
  told.mockClear()
  await expect(keepServer(a.runtime, shown, seenOf(shown))).rejects.toMatchObject({ code: 'offline_item_changed' })
  expect(told).toHaveBeenCalled()

  // The same in a tab that is not the writer.
  b.store.subscribe(told)
  told.mockClear()
  await b.writer.takeOver()
  await waitFor(() => expect(a.writer.holds()).toBe(false))
  await expect(keepServer(a.runtime, shown, seenOf(shown))).rejects.toMatchObject({ code: 'offline_not_writer' })
  expect(told).toHaveBeenCalled()
})

test('an open “Discard” question taken away because the record started sending says why', async () => {
  const { a, b } = await twoTabs({ announce: true })
  await a.store.enqueue(itemOf({ state: 'queued' }), [])
  render(<Harness runtime={a.runtime} />)
  await screen.findByText(QUEUED)
  fireEvent.click(screen.getByRole('button', { name: /Discard/ }))
  await screen.findByRole('button', { name: 'Yes, discard' })

  // The other tab's send claimed the record: this row stops offering a discard.
  await b.store.updateItem('item-1', (i) => ({ ...i, state: 'syncing' }), { bump: false })
  await screen.findByText('Syncing…')
  await screen.findByText(/question about discarding a record was closed/)
  expect(screen.queryByRole('button', { name: 'Yes, discard' })).toBeNull()
  // Nothing was removed.
  expect(await a.store.listItems()).toHaveLength(1)
})

test('an open “Discard” question taken away because this tab is not the writer any more says why', async () => {
  const { a, b } = await twoTabs({ announce: true })
  await a.store.enqueue(itemOf({ state: 'queued' }), [])
  render(<Harness runtime={a.runtime} />)
  await screen.findByText(QUEUED)
  fireEvent.click(screen.getByRole('button', { name: /Discard/ }))
  await screen.findByRole('button', { name: 'Yes, discard' })

  await b.writer.takeOver()
  await waitFor(() => expect(a.writer.holds()).toBe(false))
  await screen.findByText(/question about discarding a record was closed/)
  expect(screen.queryByRole('button', { name: 'Yes, discard' })).toBeNull()
  expect(await a.store.listItems()).toHaveLength(1)
})

test('answering the question, either way, says nothing about it being closed', async () => {
  const { a } = await twoTabs({ announce: true })
  await a.store.enqueue(itemOf({ state: 'queued' }), [])
  render(<Harness runtime={a.runtime} />)
  await screen.findByText(QUEUED)
  fireEvent.click(screen.getByRole('button', { name: /Discard/ }))
  fireEvent.click(await screen.findByRole('button', { name: 'Cancel' }))
  fireEvent.click(screen.getByRole('button', { name: /Discard/ }))
  fireEvent.click(await screen.findByRole('button', { name: 'Yes, discard' }))
  await waitFor(async () => expect(await a.store.listItems()).toEqual([]))
  await screen.findByText('Nothing is waiting to be sent.')
  expect(screen.queryByText(/question about discarding a record was closed/)).toBeNull()
})

test('an answer given in the same render that takes the row’s actions away is an answer, not a question that was closed', async () => {
  const { a } = await twoTabs({ announce: false })
  await a.store.enqueue(itemOf({ state: 'queued' }), [])
  const status = { paused: null, running: false, online: true } as const
  const apps = { [scopeKey(scope)]: 'Expenses' }
  const items = await a.store.listItems()
  const view = render(<OutboxList runtime={a.runtime} items={items} status={status} apps={apps} />)
  fireEvent.click(screen.getByRole('button', { name: /Discard/ }))
  const yes = await screen.findByRole('button', { name: 'Yes, discard' })

  // The person answers while a read that shows the record syncing lands: one render, the row has no
  // actions any more.
  act(() => {
    fireEvent.click(yes)
    view.rerender(
      <OutboxList
        runtime={a.runtime}
        items={items.map((i) => ({ ...i, state: 'syncing' as const }))}
        status={status}
        apps={apps}
      />,
    )
  })
  await screen.findByText('Syncing…')
  await waitFor(async () => expect(await a.store.listItems()).toEqual([]))
  expect(screen.queryByText(/question about discarding a record was closed/)).toBeNull()
})

const NEVER_SENT = /never sent/
const MAY_BE_THERE = /may already have reached the server/

test('after a send whose answer was lost, the question no longer says the record was never sent', async () => {
  const { a, b } = await twoTabs({ announce: false })
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  await a.store.enqueue(itemOf({ state: 'queued' }), [])
  render(<Harness runtime={a.runtime} />)
  await screen.findByText(QUEUED)

  // The other tab's send reached the server and its answer was lost: the item is back in the queue,
  // one send more, with the same version and key.
  await b.store.updateItem('item-1', (i) => ({ ...i, state: 'queued', sends: 1, unconfirmed: true }), { bump: false })

  // This tab's confirmation, made on the stale question, is refused; the list reads again.
  fireEvent.click(screen.getByRole('button', { name: /Discard/ }))
  expect(screen.queryByText(NEVER_SENT)).toBeNull()
  expect(screen.getByText(/stops any more attempts/)).toBeTruthy()
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/changed in another tab/)
  expect(await a.store.listItems()).toHaveLength(1)

  // The next question says what is true: the server may have the record.
  await waitFor(() => expect(a.store.getItem('item-1')).resolves.toMatchObject({ sends: 1 }))
  fireEvent.click(await screen.findByRole('button', { name: /Discard/ }))
  await screen.findByRole('button', { name: 'Yes, discard' })
  expect(screen.queryByText(NEVER_SENT)).toBeNull()
  expect(screen.getByText(MAY_BE_THERE)).toBeTruthy()
  expect(screen.getByText(/stops any more attempts/)).toBeTruthy()
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)
})

test('a record that was only ever queued is told what discarding does, without a claim about the server', async () => {
  const { a } = await twoTabs({ announce: false })
  await a.store.enqueue(itemOf({ state: 'queued' }), [])
  render(<Harness runtime={a.runtime} />)
  await screen.findByText(QUEUED)
  fireEvent.click(screen.getByRole('button', { name: /Discard/ }))
  await screen.findByRole('button', { name: 'Yes, discard' })
  expect(screen.queryByText(NEVER_SENT)).toBeNull()
  expect(screen.getByText(/stops any more attempts/)).toBeTruthy()
  expect(screen.queryByText(MAY_BE_THERE)).toBeNull()
})

test('an entry from before sends were counted (one attempt, no counter) says the server may have the record', async () => {
  const { a } = await twoTabs({ announce: false })
  const old = itemOf({ state: 'queued', attempts: 1 })
  delete (old as { sends?: number }).sends
  await a.store.enqueue(old, [])
  render(<Harness runtime={a.runtime} />)
  await screen.findByText(QUEUED)
  fireEvent.click(screen.getByRole('button', { name: /Discard/ }))
  await screen.findByRole('button', { name: 'Yes, discard' })
  expect(screen.queryByText(NEVER_SENT)).toBeNull()
  expect(screen.getByText(MAY_BE_THERE)).toBeTruthy()
})

test('a deletion that may have been sent says the server may already have deleted the record', async () => {
  const { a } = await twoTabs({ announce: false })
  await a.store.enqueue(itemOf({ state: 'queued', kind: 'delete', attempts: 1 }), [])
  render(<Harness runtime={a.runtime} />)
  await screen.findByText(QUEUED)
  fireEvent.click(screen.getByRole('button', { name: /Discard/ }))
  await screen.findByRole('button', { name: 'Yes, discard' })
  expect(screen.getByText(/may already have deleted the record/)).toBeTruthy()
})

test('Keep as draft on a row another tab has claimed is refused, says so, and sends nothing of its own', async () => {
  const { a, b } = await twoTabs({ announce: false })
  await a.store.enqueue(itemOf({ state: 'queued' }), [])
  render(<Harness runtime={a.runtime} />)
  await screen.findByText(QUEUED)

  // Tab B claimed the send: the item is syncing, under its lease.
  await b.store.updateItem(
    'item-1',
    (i) => ({ ...i, state: 'syncing', sends: 1, lease: { owner: 'tab-b', until: at + 60_000, attempt: 'x' } }),
    { bump: false },
  )

  fireEvent.click(screen.getByRole('button', { name: /Keep .* as a draft/ }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/changed in another tab/)
  // Not turned into a draft: the claim is B's, and the list shows where it stands.
  expect(await a.store.getItem('item-1')).toMatchObject({ state: 'syncing' })
  await screen.findByText('Syncing…')
})

test('a press on a stale row is refused when only the claim, the send count or the state moved', async () => {
  const { a, b } = await twoTabs({ announce: false })
  await a.store.enqueue(itemOf({ state: 'queued' }), [])
  const shown = (await a.store.listItems())[0] as OutboxItem
  const seen = seenAs(shown)

  // A claim that was made and given up again: same version, state and key, and a send more.
  await b.store.updateItem('item-1', (i) => ({ ...i, sends: 1 }), { bump: false })
  await expect(holdItem(a.runtime, shown, seen)).rejects.toMatchObject({ code: 'offline_item_changed' })
  await expect(retryItem(a.runtime, shown, seen)).rejects.toMatchObject({ code: 'offline_item_changed' })
  expect(await a.store.getItem('item-1')).toMatchObject({ state: 'queued', sends: 1 })

  // A lease alone.
  const withSends = seenAs((await a.store.getItem('item-1')) as OutboxItem)
  await b.store.updateItem('item-1', (i) => ({ ...i, lease: { owner: 'tab-b', until: at + 1, attempt: 'y' } }), {
    bump: false,
  })
  await expect(holdItem(a.runtime, shown, withSends)).rejects.toMatchObject({ code: 'offline_item_changed' })

  // The item as it is now is what a press acts on.
  const now = (await a.store.getItem('item-1')) as OutboxItem
  await holdItem(a.runtime, now)
  expect(await a.store.getItem('item-1')).toMatchObject({ state: 'draft' })
  const draft = (await a.store.getItem('item-1')) as OutboxItem
  await b.store.updateItem('item-1', (i) => ({ ...i, state: 'queued' }))
  await expect(releaseItem(a.runtime, draft)).rejects.toMatchObject({ code: 'offline_item_changed' })
})

test('Try again and Send on a row the other tab moved on are refused and send nothing', async () => {
  const { a, b } = await twoTabs({ announce: false })
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  await a.store.enqueue(itemOf({ state: 'queued' }), [])
  await a.store.enqueue(itemOf({ id: 'item-2', operation: 'op-00000002', state: 'draft' }), [])
  const [queued, draft] = (await a.store.listItems()) as [OutboxItem, OutboxItem]

  // The other tab synced the first and removed the second.
  await b.store.updateItem(queued.id, (i) => ({ ...i, state: 'synced' }))
  await b.store.removeItem(draft.id)
  await expect(retryItem(a.runtime, queued)).rejects.toMatchObject({ code: 'offline_item_changed' })
  await expect(releaseItem(a.runtime, draft)).rejects.toMatchObject({ code: 'offline_item_changed' })
  expect(await a.store.getItem(queued.id)).toMatchObject({ state: 'synced' })
  await a.runtime.engine.idle()
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)
})

test('Try again from a stale row says so on screen', async () => {
  const { a, b } = await twoTabs({ announce: false })
  fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  await a.store.enqueue(itemOf({ state: 'queued' }), [])
  render(<Harness runtime={a.runtime} />)
  await screen.findByText(QUEUED)
  await b.store.updateItem('item-1', (i) => ({ ...i, state: 'queued', attempts: 3 }))
  fireEvent.click(screen.getByRole('button', { name: /again now/ }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/changed in another tab/)
})

test('a “Discard” question opened from Review, closed because another tab took over, says why', async () => {
  const { a, b } = await twoTabs({ announce: true })
  await a.store.enqueue(itemOf({ state: 'attention', attention: 'refused' } as Partial<OutboxItem>), [])
  render(<Harness runtime={a.runtime} />)
  fireEvent.click(await screen.findByRole('button', { name: /^Review/ }))
  // Two Discard buttons: the row's and the one on the Review screen, which is the last.
  await waitFor(() => expect(screen.getAllByRole('button', { name: /Discard/ })).toHaveLength(2))
  fireEvent.click(screen.getAllByRole('button', { name: /Discard/ })[1] as HTMLElement)
  await screen.findByRole('button', { name: 'Yes, discard' })

  await b.writer.takeOver()
  await waitFor(() => expect(a.writer.holds()).toBe(false))
  await screen.findByText(/question about discarding a record was closed/)
  expect(screen.queryByRole('button', { name: 'Yes, discard' })).toBeNull()
  expect(await a.store.listItems()).toHaveLength(1)
})

// A sign-out of the account, certain in another tab and not resolved yet (the server hasn't
// confirmed it, or the device isn't cleared): this tab's queue sends nothing for the account
// meanwhile, neither claiming an item nor sending one it had claimed already.
test("another tab's sign-out of the account keeps this tab's queue from claiming anything", async () => {
  const { a } = await twoTabs({ announce: false })
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  await a.store.enqueue(itemOf({ state: 'queued' }), [])
  recordSignOut(7, 'login-1', false, new Map())
  await a.runtime.engine.run()
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)
  expect((await a.store.listItems())[0]?.state).toBe('queued')

  // The person went back to the app (the same login): the queue goes on.
  settleSignOut(7)
  await a.runtime.engine.run()
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(1)
})

test("a sign-out made in another tab while this tab's queue had claimed an item stops it right before it is sent", async () => {
  const { a } = await twoTabs({ announce: false })
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  await a.store.enqueue(itemOf({ state: 'queued' }), [])
  // The other tab's sign-out becomes certain just after this tab claimed the item.
  const claim = a.store.claimItem.bind(a.store)
  a.store.claimItem = async (...args: Parameters<typeof claim>) => {
    const claimed = await claim(...args)
    recordSignOut(7, 'login-1', false, new Map())
    return claimed
  }
  await a.runtime.engine.run()
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)
  // Let go, not left claimed: it is the next sender's once the sign-out is resolved.
  expect((await a.store.listItems())[0]?.state).toBe('queued')
  settleSignOut(7)
})
