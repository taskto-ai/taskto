// @vitest-environment jsdom
//
// Offline use (docs/offline.md): records made without the server survive a restart and reach it
// once, an ended session pauses sending, a write the device refuses is never shown as saved, photos
// resume after the page is reopened and the record follows them, local numbers say what they are,
// and one account never finds another's data.
//
// IndexedDB is fake-indexeddb's: jsdom has none, and what is checked (transactions that complete or
// abort, versions, a database per user) is IndexedDB's own behaviour.

import 'fake-indexeddb/auto'
import { Blob as NodeBlob } from 'node:buffer'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { ApiError, forAccount, type OfflineApp } from '../api'
import { forgetOperations } from '../apps/operations'
import { uploadPhoto } from '../apps/photos'
import { setPreferences } from '../i18n'
import { fakeFetch } from '../test/fakes'
import { downloadApp } from './download'
import { LogoutGuard } from './LogoutGuard'
import { itemsOf } from './local'
import { OfflinePanel } from './OfflinePanel'
import { OfflineApp as OfflineAppView } from './OfflineShell'
import { revalidate } from './revalidate'
import { activate, deactivate, discardForeign, lastUser, rememberUser, runtimeState, setSyncDeps } from './runtime'
import { type Download, NotSaved, OfflineStore, type OutboxItem, scopeKey } from './store'
import { backoff, LOCK_RETRY_MS, MAX_ATTEMPTS, SyncEngine } from './sync'
import { localPhotos, offlineTransport } from './transport'

const scope = { workspace: 1, space: 2, app: 1 }
const base = '/api/workspaces/1/spaces/2/apps/1'

const definition: OfflineApp['definition'] = {
  definition_version: 1,
  requires: [],
  collection: {
    fields: [
      { name: 'description', type: 'string', required: true, max_length: 200 },
      { name: 'amount', type: 'number', required: true },
    ],
  },
  actions: { add_expense: { kind: 'insert' }, delete_expense: { kind: 'delete' } },
  summaries: { total: { kind: 'sum', field: 'amount' } },
}

function download(user: number, patch: Partial<Download> = {}): Download {
  return {
    ...scope,
    key: scopeKey(scope),
    user,
    name: 'Expenses',
    release: 3,
    definition,
    messages: surface,
    scopeOf: 'all',
    records: 1,
    total: 1,
    truncated: false,
    canInsert: true,
    capturedAt: '2026-11-02T10:00:00.000Z',
    bytes: 100,
    ...patch,
    operations: patch.operations ?? {},
  }
}

function offlineApp(patch: Partial<OfflineApp> = {}): OfflineApp {
  return {
    app_id: 1,
    space_id: 2,
    name: 'Expenses',
    release: 3,
    definition,
    messages: surface,
    scope: 'all',
    records: 1,
    total: 1,
    truncated: false,
    can_insert: true,
    captured_at: '2026-11-02T10:00:00.000Z',
    ...patch,
  }
}

const press = (name: string, context: Record<string, unknown>) => ({
  name,
  surfaceId: 'app-1',
  sourceComponentId: 'add',
  timestamp: '2026-11-02T10:00:00.000Z',
  context,
})

beforeEach(async () => {
  // Every test starts with a browser that has never been used.
  globalThis.indexedDB = new IDBFactory()
  window.localStorage.clear()
  forgetOperations()
  await setPreferences({})
})

afterEach(async () => {
  cleanup()
  Reflect.deleteProperty(navigator, 'storage')
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  setSyncDeps({})
  await deactivate({ purge: false })
})

// jsdom's Blob can't be copied into the fake database, Node's can: the same thing to the code under test.
const blob = (data: string, type = '') => new NodeBlob([data], { type }) as unknown as Blob

// The browser's storage manager, which jsdom has none of.
function stubStorage(storage: Record<string, unknown>) {
  Object.defineProperty(navigator, 'storage', { value: storage, configurable: true })
}

// What the engine uses instead of the clock, the timers and the network's retries.
function clock() {
  let at = 1_000_000
  return {
    now: () => at,
    advance: (ms: number) => {
      at += ms
    },
  }
}

async function openStore(user = 7) {
  return OfflineStore.open(user)
}

function engineOf(store: OfflineStore, c = clock(), extra: ConstructorParameters<typeof SyncEngine>[1] = {}) {
  return new SyncEngine(store, { now: c.now, setTimer: () => 0, clearTimer: () => {}, random: () => 0.5, ...extra })
}

// An engine that sends nothing: the screen's button asks it to run, and the tests decide when.
function idle(store: OfflineStore) {
  return { user: store.user, store, engine: { run: async () => {} } as unknown as SyncEngine, stop: () => {} }
}

// Queues an insert the way the offline screen's button does, through the transport.
async function queue(store: OfflineStore, context: Record<string, unknown>, d = download(store.user)) {
  const runtime = idle(store)
  await store.putDownload(d)
  const transport = offlineTransport(runtime, d)
  await transport.act(press('add_expense', context))
  return transport
}

test('an offline record is on the device before it is shown as saved, and survives a restart', async () => {
  const store = await openStore()
  await queue(store, { description: 'Lunch', amount: '12.50' })
  const [item] = await store.listItems()
  expect(item).toMatchObject({
    state: 'queued',
    user: 7,
    workspace: 1,
    space: 2,
    app: 1,
    release: 3,
    schema: 1,
    attempts: 0,
    attachments: [],
  })
  expect(item.operation).toMatch(/^[A-Za-z0-9_-]{8,64}$/)
  expect(item.record.id).toBeLessThan(0)
  // "Restart": the connection is closed and a new page opens the database again.
  store.close()
  const reopened = await openStore()
  const [again] = await reopened.listItems()
  expect(again.id).toBe(item.id)
  expect(again.operation).toBe(item.operation)
  expect(again.action.context).toEqual({ description: 'Lunch', amount: '12.50' })
  expect((await reopened.listDownloads()).map((d) => d.key)).toEqual([scopeKey(scope)])
})

test('a retried record is made once: the same operation key, and nothing is sent after the server has it', async () => {
  const store = await openStore()
  await queue(store, { description: 'Lunch', amount: '12.50' })
  const c = clock()
  let up = false
  let made = 0
  const calls = fakeFetch({
    [`POST ${base}/actions`]: (call) => {
      if (!up) throw new TypeError('network down')
      // The server's side: a key it has seen changes nothing.
      const op = (call.body as { operation: string }).operation
      seen.add(op)
      made = seen.size
      return { body: [] }
    },
  })
  const seen = new Set<string>()
  const engine = engineOf(store, c)
  await engine.run()
  // The answer never came: the record waits, with a time to try again.
  let [item] = await store.listItems()
  expect(item).toMatchObject({ state: 'queued', attempts: 1 })
  expect(item.nextAttemptAt).toBeGreaterThan(c.now())
  // Not yet due: nothing is sent.
  await engine.run()
  expect(calls).toHaveLength(1)
  up = true
  c.advance(60_000)
  await engine.run()
  ;[item] = await store.listItems()
  expect(item.state).toBe('synced')
  expect(item.syncedAt).toBeTruthy()
  // Both sends are the same request, and the server made one record.
  const ops = calls.filter((x) => x.method === 'POST').map((x) => (x.body as { operation: string }).operation)
  expect(ops).toHaveLength(2)
  expect(new Set(ops).size).toBe(1)
  expect(ops[0]).toBe(item.operation)
  expect(made).toBe(1)
  expect((calls[1].body as { release: number }).release).toBe(3)
  // Synced: it is never sent again, by any later pass or by a new page.
  await engine.run()
  await engineOf(await openStore(), c).run()
  expect(calls.filter((x) => x.method === 'POST')).toHaveLength(2)
})

test('the server answering a record whose answer was lost is the same as the first: synced, not duplicated', async () => {
  const store = await openStore()
  await queue(store, { description: 'Taxi', amount: '8' })
  let first = true
  const seen = new Set<string>()
  fakeFetch({
    [`POST ${base}/actions`]: (call) => {
      seen.add((call.body as { operation: string }).operation)
      if (first) {
        first = false
        // Applied, but the connection dropped before the answer.
        throw new TypeError('connection reset')
      }
      return { body: [] }
    },
  })
  const c = clock()
  const engine = engineOf(store, c)
  await engine.run()
  c.advance(60_000)
  await engine.run()
  expect((await store.listItems())[0].state).toBe('synced')
  expect(seen.size).toBe(1)
})

test('transient failures back off, and stop retrying for ever: the person is asked', async () => {
  expect(backoff(1, () => 0.5)).toBe(2000)
  expect(backoff(2, () => 0.5)).toBe(4000)
  expect(backoff(20, () => 0.5)).toBe(5 * 60_000)
  const store = await openStore()
  await queue(store, { description: 'Lunch', amount: '1' })
  const calls = fakeFetch({ [`POST ${base}/actions`]: { status: 503, body: { error: 'busy' } } })
  const c = clock()
  const engine = engineOf(store, c)
  for (let i = 0; i < MAX_ATTEMPTS + 2; i++) {
    c.advance(10 * 60_000)
    await engine.run()
  }
  const [item] = await store.listItems()
  expect(item).toMatchObject({ state: 'attention', attention: 'gave_up', attempts: MAX_ATTEMPTS })
  expect(calls).toHaveLength(MAX_ATTEMPTS)
  // The input is kept, and "try again now" is the person's call.
  expect(item.action.context).toEqual({ description: 'Lunch', amount: '1' })
  fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  await engine.retry(item.id)
  expect((await store.listItems())[0].state).toBe('synced')
})

test('an ended session pauses sending, loses nothing, and resumes once someone signs in', async () => {
  const store = await openStore()
  await queue(store, { description: 'Lunch', amount: '12' })
  let signedIn = false
  const calls = fakeFetch({
    [`POST ${base}/actions`]: () => (signedIn ? { body: [] } : { status: 401, body: { error: 'sign in' } }),
  })
  const engine = engineOf(store)
  const stop = engine.start()
  await waitFor(() => expect(engine.status().paused).toBe('session'))
  let [item] = await store.listItems()
  // Not a failed attempt, not an error: it waits for the session.
  expect(item).toMatchObject({ state: 'queued', attempts: 0 })
  expect(item.error).toBeUndefined()
  // Nothing is sent while it is paused, whatever happens to the network.
  const sent = calls.length
  window.dispatchEvent(new Event('online'))
  await engine.run()
  expect(calls).toHaveLength(sent)
  signedIn = true
  engine.resume()
  await waitFor(async () => expect((await store.listItems())[0].state).toBe('synced'))
  ;[item] = await store.listItems()
  expect(item.operation).toBe((calls[calls.length - 1].body as { operation: string }).operation)
  stop()
})

test('the server refusing for good, or access lost, needs attention at once and keeps the input', async () => {
  const store = await openStore()
  await queue(store, { description: 'A', amount: '1' })
  await queue(store, { description: 'B', amount: '2' })
  fakeFetch({
    [`POST ${base}/actions`]: (call) =>
      (call.body as { context: { description: string } }).context.description === 'A'
        ? {
            status: 422,
            body: { error: 'amount too small', code: 'app_field_too_small', params: { field: 'amount', min: 5 } },
          }
        : { status: 409, body: { error: 'archived', code: 'space_archived' } },
  })
  const engine = engineOf(store)
  await engine.run()
  const [a, b] = await store.listItems()
  expect(a).toMatchObject({ state: 'attention', attention: 'refused', attempts: 0 })
  expect(a.error?.code).toBe('app_field_too_small')
  expect(b).toMatchObject({ state: 'attention', attention: 'archived' })
  expect(a.action.context).toEqual({ description: 'A', amount: '1' })
  // A refusal is not retried until the person says so.
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  await engine.run()
  expect(calls).toHaveLength(0)
})

test('confirmed revocation: replay stops, the downloaded copy is removed, the input stays', async () => {
  const store = await openStore()
  await queue(store, { description: 'A', amount: '1' })
  const removed: string[] = []
  const calls = fakeFetch({
    [`POST ${base}/actions`]: { status: 404, body: { error: 'not found', code: 'not_found' } },
    [`GET ${base}`]: { status: 404, body: { error: 'not found', code: 'not_found' } },
  })
  const engine = new SyncEngine(store, {
    now: clock().now,
    setTimer: () => 0,
    onRevoked: async (item) => {
      removed.push(item.scopeKey)
      await store.removeDownload(item)
    },
  })
  await engine.run()
  expect(removed).toEqual([scopeKey(scope)])
  expect(await store.listDownloads()).toEqual([])
  const [item] = await store.listItems()
  expect(item).toMatchObject({ state: 'attention', attention: 'access' })
  expect(item.action.context.description).toBe('A')
  // Replay is blocked: a later pass sends nothing.
  const before = calls.length
  await engine.run()
  expect(calls).toHaveLength(before)
})

test('a 403 with the app still reachable is a refusal of this record, not a revocation', async () => {
  const store = await openStore()
  await queue(store, { description: 'A', amount: '1' })
  fakeFetch({
    [`POST ${base}/actions`]: {
      status: 403,
      body: { error: 'no', code: 'app_forbidden', params: { capability: 'submit' } },
    },
    [`GET ${base}`]: { body: { id: 1 } },
  })
  await engineOf(store).run()
  expect(await store.listDownloads()).toHaveLength(1)
  expect((await store.listItems())[0]).toMatchObject({ state: 'attention', attention: 'access' })
})

test('revalidating removes a copy the server no longer allows, and keeps one when the network is down', async () => {
  const store = await openStore()
  await store.putDownload(download(7))
  fakeFetch({})
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => Promise.reject(new TypeError('offline'))),
  )
  expect(await revalidate(store)).toEqual([])
  expect(await store.listDownloads()).toHaveLength(1)
  // An admin turned downloads off for the space.
  fakeFetch({ 'GET /api/workspaces/1/spaces/2': { body: { id: 2, offline_downloads: false } } })
  expect((await revalidate(store)).map((d) => d.key)).toEqual([scopeKey(scope)])
  expect(await store.listDownloads()).toEqual([])
})

test('a write the device refuses is never shown as saved, and the input is still there', async () => {
  const store = await openStore()
  const d = download(7)
  await store.putDownload(d)
  const runtime = idle(store)
  // The storage is full: the write to the queue throws what a browser throws.
  const put = IDBObjectStore.prototype.put
  vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (this: IDBObjectStore, ...args) {
    if (this.name === 'outbox') throw new DOMException('The quota has been exceeded.', 'QuotaExceededError')
    return put.apply(this, args as Parameters<typeof put>)
  })
  const saved = vi.fn()
  render(<OfflineAppView runtime={runtime} download={d} items={[]} />)
  void saved
  await screen.findByText('Expense tracker')
  fireEvent.input(inputOf('Description'), { target: { value: 'Lunch' } })
  fireEvent.input(inputOf('Amount'), { target: { value: '12.25' } })
  fireEvent.click(screen.getByRole('button', { name: 'Add' }))
  const alert = await screen.findByRole('alert')
  expect(alert.textContent).toMatch(/NOT saved/)
  expect(alert.textContent).toMatch(/no room left/)
  // Nothing says it was saved, nothing was kept, and the form still holds what was typed.
  expect(screen.queryByText(/Saved on this device/)).toBeNull()
  expect(await store.listItems()).toEqual([])
  expect(inputOf('Description').value).toBe('Lunch')
  // The store says so too, to whoever asked.
  const transport = offlineTransport(runtime, d)
  await expect(transport.act(press('add_expense', { description: 'x', amount: '1' }))).rejects.toThrow(/NOT saved/)
  await expect(store.enqueue({ ...(await fakeItem(store)) }, [])).rejects.toBeInstanceOf(NotSaved)
  await expect(store.enqueue({ ...(await fakeItem(store)) }, [])).rejects.toMatchObject({ reason: 'quota' })
})

async function fakeItem(store: OfflineStore): Promise<Omit<OutboxItem, 'seq'>> {
  const now = new Date().toISOString()
  return {
    id: 'x',
    user: store.user,
    scopeKey: scopeKey(scope),
    ...scope,
    base,
    release: 1,
    schema: 1,
    action: { name: 'add_expense', surfaceId: 'app-1', sourceComponentId: 'add', timestamp: now, context: {} },
    operation: 'op-0000000001',
    record: { id: -1, revision: null },
    attachments: [],
    state: 'queued',
    attempts: 0,
    createdAt: now,
    updatedAt: now,
  }
}

function inputOf(label: string): HTMLInputElement {
  const input = screen.getByText(label).closest('a2ui-basic-textfield')?.querySelector('input')
  if (!input) throw new Error(`no input for ${label}`)
  return input
}

function stat(label: string): string {
  return screen.getByText(label).closest('dl')?.querySelector('dd')?.textContent ?? ''
}

test('the offline screen is the downloaded release, its numbers say what they cover, and "saved" follows the write', async () => {
  const store = await openStore()
  const d = download(7)
  await store.putDownload(d)
  const runtime = idle(store)
  const items: OutboxItem[] = []
  render(<OfflineAppView runtime={runtime} download={d} items={items} />)
  await screen.findByText('Expense tracker')
  expect(stat('Total spent')).toBe('4.50')
  // Local numbers are labelled as local: never the server's totals.
  const note = screen.getByRole('note')
  expect(note.textContent).toMatch(/Offline copy/)
  expect(note.textContent).toMatch(/not the server’s totals/)
  expect(note.textContent).toMatch(/downloaded: 1/)
  expect(note.textContent).toMatch(/saved here: 0/)

  fireEvent.input(inputOf('Description'), { target: { value: 'Lunch' } })
  fireEvent.input(inputOf('Amount'), { target: { value: '12.25' } })
  fireEvent.click(screen.getByRole('button', { name: 'Add' }))
  await screen.findByText('Lunch')
  // The sum covers the downloaded record and the one queued here, and nothing else.
  expect(stat('Total spent')).toBe('16.75')
  expect((await screen.findByRole('status')).textContent).toMatch(/Saved on this device/)
  const [item] = await store.listItems()
  expect(item.action.context).toEqual({ description: 'Lunch', amount: '12.25' })
  // A record that fails the app's own rules is refused here, with the server's words, and not queued.
  fireEvent.input(inputOf('Description'), { target: { value: 'No amount' } })
  fireEvent.input(inputOf('Amount'), { target: { value: 'abc' } })
  fireEvent.click(screen.getByRole('button', { name: 'Add' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/must be a number|amount/i)
  expect(await store.listItems()).toHaveLength(1)
})

test('what needs the server stays online: a button that changes or deletes a record is refused offline', async () => {
  const store = await openStore()
  const d = download(7)
  const runtime = idle(store)
  const transport = offlineTransport(runtime, d)
  await expect(transport.act(press('delete_expense', { id: 1 }))).rejects.toMatchObject({ code: 'offline_unsupported' })
  await expect(transport.act(press('nope', {}))).rejects.toBeInstanceOf(ApiError)
  expect(await store.listItems()).toEqual([])
})

test('a release this client cannot draw shows the update-required state, not a broken app', async () => {
  const store = await openStore()
  const d = download(7, {
    definition: { ...definition, definition_version: 99, requires: ['component.FromTheFuture'] },
  })
  const runtime = idle(store)
  render(<OfflineAppView runtime={runtime} download={d} items={[]} />)
  expect((await screen.findByRole('status')).textContent).toMatch(/update/i)
  expect(screen.queryByText('Expense tracker')).toBeNull()
})

test('a photo is kept with its record, uploaded first, and resumes after the page is reopened; the record waits for it', async () => {
  const store = await openStore()
  const photoDefinition: OfflineApp['definition'] = {
    ...definition,
    collection: {
      fields: [{ name: 'description', type: 'string' }, { name: 'photos', type: 'attachment', max_files: 2 } as never],
    },
  }
  const d = download(7, { definition: photoDefinition })
  await store.putDownload(d)
  const runtime = idle(store)
  const transport = offlineTransport(runtime, d)
  const photo = blob('not really a picture', 'image/png')
  const localId = transport.photos.stage(photo)
  await transport.act(press('add_expense', { description: 'Receipt', photos: [localId] }))
  const [item] = await store.listItems()
  expect(item.attachments).toEqual([
    expect.objectContaining({ localId, field: 'photos', state: 'pending', size: photo.size, type: 'image/png' }),
  ])
  // The bytes are on the device with the record, not in memory.
  expect(await (await store.getBlob(item.attachments[0].blobId))?.text()).toBe('not really a picture')

  // First pass: the upload starts, and the connection drops.
  const order: string[] = []
  fakeFetch({
    [`POST ${base}/actions`]: (call) => {
      order.push('record')
      expect((call.body as { context: { photos: string[] } }).context.photos).toEqual(['f'.repeat(32)])
      return { body: [] }
    },
  })
  const c = clock()
  let attempt = 0
  const upload = vi.fn(
    async (
      _base: string,
      _field: string,
      blob: Blob,
      opts: { resume?: string; onStarted?: (id: string) => void } = {},
    ) => {
      order.push(`upload${opts.resume ? ` resumed ${opts.resume}` : ''}`)
      expect(blob.size).toBe(photo.size)
      opts.onStarted?.('u'.repeat(32))
      if (attempt++ === 0) throw new TypeError('network down')
      return {
        id: 'f'.repeat(32),
        field: 'photos',
        status: 'ready' as const,
        size: blob.size,
        received: blob.size,
        attached: false,
      }
    },
  )
  await engineOf(store, c, { upload }).run()
  let [after] = await store.listItems()
  expect(after.state).toBe('queued')
  // The record is NOT sent while its photo is not on the server.
  expect(order).toEqual(['upload'])
  expect(after.attachments[0]).toMatchObject({ uploadId: 'u'.repeat(32), state: 'uploading' })

  // The page is closed and opened again: a new store and engine find the upload and go on from it.
  store.close()
  const reopened = await openStore()
  c.advance(60_000)
  await engineOf(reopened, c, { upload }).run()
  ;[after] = await reopened.listItems()
  expect(order).toEqual(['upload', `upload resumed ${'u'.repeat(32)}`, 'record'])
  expect(after.state).toBe('synced')
  expect(after.attachments[0]).toMatchObject({ state: 'uploaded', serverId: 'f'.repeat(32) })
  // The server has the photo, so the copy is let go.
  expect(await reopened.getBlob(after.attachments[0].blobId)).toBeUndefined()
})

test('a photo the server refuses is uploaded again when the person tries again, never silently skipped', async () => {
  const store = await openStore()
  const d = download(7, {
    definition: { ...definition, collection: { fields: [{ name: 'photos', type: 'attachment' } as never] } },
  })
  await store.putDownload(d)
  const runtime = idle(store)
  const transport = offlineTransport(runtime, d)
  await transport.act(press('add_expense', { photos: [transport.photos.stage(blob('x', 'image/png'))] }))
  let reply: { status?: number; body: unknown } = {
    status: 400,
    body: { error: 'bad photo', code: 'app_attachment_bad' },
  }
  fakeFetch({ [`POST ${base}/actions`]: () => reply })
  const upload = vi.fn(async () => ({
    id: 'a'.repeat(32),
    field: 'photos',
    status: 'ready' as const,
    size: 1,
    received: 1,
    attached: false,
  }))
  const engine = engineOf(store, clock(), { upload })
  await engine.run()
  let [item] = await store.listItems()
  expect(item).toMatchObject({ state: 'attention', attention: 'photo' })
  expect(item.attachments[0].state).toBe('pending')
  reply = { body: [] }
  await engine.retry(item.id)
  ;[item] = await store.listItems()
  expect(item.state).toBe('synced')
  expect(upload).toHaveBeenCalledTimes(2)
})

test('local drafts are held on the device and sent on purpose', async () => {
  const store = await openStore()
  await queue(store, { description: 'Later', amount: '3' })
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  const engine = engineOf(store)
  const [item] = await store.listItems()
  await engine.hold(item.id)
  expect((await store.listItems())[0].state).toBe('draft')
  await engine.run()
  expect(calls).toHaveLength(0)
  await engine.release(item.id)
  expect((await store.listItems())[0].state).toBe('synced')
})

test('another account on this browser finds none of the last one’s data', async () => {
  // Alice downloads an app and has a record that has not reached the server.
  await activate(1, 'Alice')
  const alice = runtimeState().runtime
  expect(alice).not.toBeNull()
  if (!alice) return
  await alice.store.putDownload(download(1))
  await queue(alice.store, { description: 'Alice’s secret', amount: '1' }, download(1))
  alice.engine.pause('manual')
  // Bob signs in on the same browser.
  await activate(2, 'Bob')
  const bob = runtimeState().runtime
  expect(bob?.user).toBe(2)
  expect(await bob?.store.listDownloads()).toEqual([])
  expect(await bob?.store.listItems()).toEqual([])
  expect(lastUser()).toEqual({ id: 2, name: 'Bob' })
  // Alice's copy of the app is gone (only her unsent input remains, sealed under her own id)...
  const sealed = await OfflineStore.open(1)
  expect(await sealed.listDownloads()).toEqual([])
  expect(await sealed.listItems()).toHaveLength(1)
  sealed.close()
  // ...and nothing of hers is readable through Bob's database.
  const names = (await indexedDB.databases()).map((x) => x.name)
  expect(names).toContain('taskto-offline-2')
  await bob?.store.putDownload(download(2))
  expect(JSON.stringify(await bob?.store.listDownloads())).not.toContain('Alice')

  // Alice with nothing unsent: her whole database is deleted when Bob signs in.
  await deactivate({ purge: true })
  await activate(3, 'Carol')
  const carol = runtimeState().runtime
  await carol?.store.putDownload(download(3))
  await activate(4, 'Dan')
  const left = (await indexedDB.databases()).map((x) => x.name)
  expect(left).not.toContain('taskto-offline-3')
  expect(left).toContain('taskto-offline-4')
})

test('signing out deletes the account’s database, the downloads and the queue with it', async () => {
  await activate(7, 'Pat')
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  await runtime.store.putDownload(download(7))
  await queue(runtime.store, { description: 'x', amount: '1' }, download(7))
  expect((await indexedDB.databases()).map((x) => x.name)).toContain('taskto-offline-7')
  await deactivate({ purge: true })
  expect((await indexedDB.databases()).map((x) => x.name)).not.toContain('taskto-offline-7')
  expect(lastUser()).toBeNull()
  expect(runtimeState().runtime).toBeNull()
  // Signing in again starts from nothing.
  await activate(7, 'Pat')
  expect(await runtimeState().runtime?.store.listDownloads()).toEqual([])
  expect(await runtimeState().runtime?.store.listItems()).toEqual([])
})

test('signing out with records not sent offers to sync or discard, and syncing sends them first', async () => {
  const store = await openStore()
  await queue(store, { description: 'A', amount: '1' })
  const engine = engineOf(store)
  const runtime = { user: 7, store, engine, stop: () => {} }
  let up = false
  const calls = fakeFetch({
    [`POST ${base}/actions`]: () => (up ? { body: [] } : { status: 503, body: { error: 'busy' } }),
  })
  const out = vi.fn()
  const cancel = vi.fn()
  render(<LogoutGuard runtime={runtime} count={1} onSignOut={out} onCancel={cancel} />)
  expect(screen.getByRole('alertdialog').textContent).toMatch(/1 record on this device hasn’t reached the server/)
  fireEvent.click(screen.getByRole('button', { name: 'Sync now' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/still waiting/)
  expect(out).not.toHaveBeenCalled()
  up = true
  fireEvent.click(screen.getByRole('button', { name: 'Sync now' }))
  await waitFor(() => expect(out).toHaveBeenCalledOnce())
  expect(calls.filter((c) => c.method === 'POST').length).toBeGreaterThanOrEqual(2)
  // Discard is a second, explicit step; cancel leaves everything.
  out.mockClear()
  fireEvent.click(screen.getByRole('button', { name: 'Discard and sign out' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  expect(out).toHaveBeenCalledOnce()
  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(cancel).toHaveBeenCalledOnce()
})

test('a database written by a newer version is left alone, and the page asks to be updated', async () => {
  await new Promise<void>((resolve, reject) => {
    const open = indexedDB.open('taskto-offline-9', 99)
    open.onupgradeneeded = () => open.result.createObjectStore('future')
    open.onsuccess = () => {
      open.result.close()
      resolve()
    }
    open.onerror = () => reject(open.error)
  })
  const state = await activate(9, 'Future')
  expect(state).toMatchObject({ runtime: null, problem: 'schema' })
  // Untouched: still version 99, with its store.
  const version = await new Promise<number>((resolve) => {
    const open = indexedDB.open('taskto-offline-9')
    open.onsuccess = () => {
      const v = open.result.version
      open.result.close()
      resolve(v)
    }
  })
  expect(version).toBe(99)
})

test('downloading keeps the release and the records, asks the browser to keep them, and says what it is', async () => {
  const store = await openStore()
  const persist = vi.fn(async () => true)
  stubStorage({ persist, estimate: async () => ({ usage: 1e6, quota: 5e8 }), persisted: async () => false })
  fakeFetch({ [`GET ${base}/offline`]: { body: offlineApp({ records: 1, total: 40, truncated: true, scope: 'own' }) } })
  const d = await downloadApp(store, scope)
  expect(d).toMatchObject({ release: 3, records: 1, total: 40, truncated: true, scopeOf: 'own', canInsert: true })
  expect((await store.getDownload(scope))?.capturedAt).toBe('2026-11-02T10:00:00.000Z')
  await waitFor(() => expect(persist).toHaveBeenCalled())
})

test('the panel downloads, shows readiness and storage, and says what stays online', async () => {
  await activate(7, 'Pat')
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  stubStorage({
    persist: async () => false,
    estimate: async () => ({ usage: 2e6, quota: 4e9 }),
    persisted: async () => false,
  })
  fakeFetch({ [`GET ${base}/offline`]: { body: offlineApp() } })
  const app = { id: 1, name: 'Expenses', live: { number: 5, definition_version: 1, requires: [] } } as never
  render(<OfflinePanel workspace={1} space={2} app={app} spaceInfo={{ offline_downloads: true } as never} />)
  const panel = await screen.findByRole('region', { name: 'Offline use' })
  expect(within(panel).getByText(/Not downloaded yet/)).toBeTruthy()
  expect(within(panel).getByText(/Building with AI, publishing and sharing need a connection/)).toBeTruthy()
  fireEvent.click(within(panel).getByRole('button', { name: 'Download Expenses for offline use' }))
  expect(await within(panel).findByText('Ready for offline use')).toBeTruthy()
  expect(panel.textContent).toMatch(/Release 3, downloaded/)
  expect(panel.textContent).toMatch(/1 of 1 records are on this device/)
  // A newer live release than the copy is said.
  expect(panel.textContent).toMatch(/A newer release \(number 5\) is live/)
  await waitFor(() => expect(panel.textContent).toMatch(/Storage on this device: about .* used of/))
  expect(panel.textContent).toMatch(/may clear downloaded data/)
})

test('an admin who turned downloads off: the panel offers none, and the server’s refusal is shown', async () => {
  await activate(7, 'Pat')
  const app = { id: 1, name: 'Expenses', live: { number: 3, definition_version: 1, requires: [] } } as never
  const { unmount } = render(
    <OfflinePanel workspace={1} space={2} app={app} spaceInfo={{ offline_downloads: false } as never} />,
  )
  const panel = await screen.findByRole('region', { name: 'Offline use' })
  expect(within(panel).getByText(/turned off offline downloads for this space/)).toBeTruthy()
  expect(within(panel).queryByRole('button', { name: /Download/ })).toBeNull()
  unmount()
  // The panel didn't know (an older list): the server refuses, and says so in words.
  fakeFetch({ [`GET ${base}/offline`]: { status: 403, body: { error: 'off', code: 'offline_downloads_disabled' } } })
  render(<OfflinePanel workspace={1} space={2} app={app} spaceInfo={{ offline_downloads: true } as never} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Download Expenses for offline use' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/turned off offline downloads/)
  expect(await runtimeState().runtime?.store.listDownloads()).toEqual([])
})

test('a quota failure while downloading is said, and no copy is kept as if it were', async () => {
  await activate(7, 'Pat')
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  const put = IDBObjectStore.prototype.put
  vi.spyOn(IDBObjectStore.prototype, 'put').mockImplementation(function (this: IDBObjectStore, ...args) {
    if (this.name === 'downloads') throw new DOMException('full', 'QuotaExceededError')
    return put.apply(this, args as Parameters<typeof put>)
  })
  fakeFetch({ [`GET ${base}/offline`]: { body: offlineApp() } })
  const app = { id: 1, name: 'Expenses', live: { number: 3, definition_version: 1, requires: [] } } as never
  render(<OfflinePanel workspace={1} space={2} app={app} spaceInfo={{ offline_downloads: true } as never} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Download Expenses for offline use' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/NOT saved/)
  expect(screen.queryByText('Ready for offline use')).toBeNull()
  expect(await runtime.store.listDownloads()).toEqual([])
})

test('staged photos are shown from memory and forgotten when removed', () => {
  const photos = localPhotos()
  const id = photos.stage(blob('x'))
  expect(id).toMatch(/^[0-9a-f]{32}$/)
  expect(photos.take(id)).toBeDefined()
  photos.forget(id)
  expect(photos.take(id)).toBeUndefined()
  expect(photos.url(id)).toBeUndefined()
})

// --- Review fixes: accounts, replay, revocation

const EXPECTED = 'x-taskto-expected-user'
const wrongAccount = { status: 409, body: { error: 'another account', code: 'session_account_changed' } }

test('a queue is sent only as its owner: every request names the account, and another one pauses it', async () => {
  const store = await openStore(7)
  await queue(store, { description: 'Alice’s', amount: '1' })
  // The browser's marker says someone else is signed in: nothing is asked of the server.
  let who: number | null = 8
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  const engine = engineOf(store, clock(), { signedIn: () => who })
  await engine.run()
  expect(calls).toHaveLength(0)
  expect(engine.status().paused).toBe('account')
  expect((await store.listItems())[0]).toMatchObject({ state: 'queued', attempts: 0 })
  // Her again: sending goes on, and the request says whose it is.
  who = 7
  engine.resume()
  await waitFor(async () => expect((await store.listItems())[0].state).toBe('synced'))
  expect(calls[0].headers[EXPECTED]).toBe('7')
})

test('the server refusing the session as another account pauses sending, and is never the item’s failure', async () => {
  const store = await openStore(7)
  await queue(store, { description: 'Alice’s', amount: '1' })
  const calls = fakeFetch({ [`POST ${base}/actions`]: wrongAccount })
  const engine = engineOf(store)
  await engine.run()
  expect(calls).toHaveLength(1)
  expect(calls[0].headers[EXPECTED]).toBe('7')
  expect(engine.status().paused).toBe('account')
  const [item] = await store.listItems()
  expect(item).toMatchObject({ state: 'queued', attempts: 0 })
  expect(item.error).toBeUndefined()
  // The same for a photo: its upload is refused as another account's, and the record waits.
  const photoDef = { ...definition, collection: { fields: [{ name: 'photos', type: 'attachment' } as never] } }
  const second = await openStore(8)
  const d = download(8, { definition: photoDef })
  await second.putDownload(d)
  const transport = offlineTransport(idle(second), d)
  await transport.act(press('add_expense', { photos: [transport.photos.stage(blob('x', 'image/png'))] }))
  const upload = vi.fn(async () => {
    throw new ApiError(409, 'another account', { code: 'session_account_changed' })
  })
  const photos = engineOf(second, clock(), { upload })
  await photos.run()
  expect(photos.status().paused).toBe('account')
  const [queued] = await second.listItems()
  expect(queued).toMatchObject({ state: 'queued', attempts: 0 })
  expect(queued.attention).toBeUndefined()
})

test('an upload carries the account it is for on every request', async () => {
  const calls = fakeFetch({
    [`POST ${base}/uploads`]: {
      status: 201,
      body: { id: 'u'.repeat(32), status: 'uploading', received: 0, size: 1, chunk_size: 10 },
    },
    [`PUT ${base}/uploads/${'u'.repeat(32)}?offset=0`]: {
      body: { id: 'u'.repeat(32), status: 'uploading', received: 1, size: 1 },
    },
    [`POST ${base}/uploads/${'u'.repeat(32)}/finalize`]: {
      body: { id: 'u'.repeat(32), status: 'ready', received: 1, size: 1 },
    },
  })
  await uploadPhoto(base, 'photos', blob('x', 'image/png'), { headers: forAccount(7) })
  expect(calls).toHaveLength(3)
  for (const c of calls) expect(c.headers[EXPECTED]).toBe('7')
})

test('a database that can’t be opened never shows the last account’s: the marker moves first, and the error is said', async () => {
  // Alice has unsent work on this browser.
  await activate(1, 'Alice')
  const alice = runtimeState().runtime
  if (!alice) throw new Error('no runtime')
  await queue(alice.store, { description: 'Alice’s secret', amount: '1' }, download(1))
  await deactivate({ purge: false })
  // Bob's database is of a newer version than this page.
  await new Promise<void>((resolve, reject) => {
    const open = indexedDB.open('taskto-offline-2', 99)
    open.onupgradeneeded = () => open.result.createObjectStore('future')
    open.onsuccess = () => {
      open.result.close()
      resolve()
    }
    open.onerror = () => reject(open.error)
  })
  window.localStorage.setItem('taskto:offline-users', '[1,2]')
  expect(await activate(2, 'Bob')).toMatchObject({ runtime: null, problem: 'schema' })
  // The browser is Bob's now, so an offline start opens his (and fails the same), never hers.
  expect(lastUser()).toEqual({ id: 2, name: 'Bob' })
  const before = lastUser()
  if (!before) throw new Error('no marker')
  expect(await activate(before.id)).toMatchObject({ runtime: null, problem: 'schema' })
  expect(runtimeState().runtime).toBeNull()
})

test('another account signing in: the last one’s downloads, synced records and photos go at once, unsent ones wait for a choice', async () => {
  await activate(1, 'Alice')
  const alice = runtimeState().runtime
  if (!alice) throw new Error('no runtime')
  alice.engine.pause('manual')
  await alice.store.putDownload(download(1))
  await queue(alice.store, { description: 'Sent already', amount: '1' }, download(1))
  await queue(alice.store, { description: 'Not sent', amount: '2' }, download(1))
  const [sent] = await alice.store.listItems()
  await alice.store.updateItem(sent.id, (i) => ({ ...i, state: 'synced' }))
  // Bob signs in on the same browser.
  const state = await activate(2, 'Bob')
  expect(state.foreign).toEqual({ users: [1], count: 1 })
  expect(state.runtime?.user).toBe(2)
  expect(await state.runtime?.store.listItems()).toEqual([])
  // Of hers only the unsent record is left: nothing downloaded, nothing already sent.
  const sealed = await OfflineStore.open(1)
  expect(await sealed.listDownloads()).toEqual([])
  expect((await sealed.listItems()).map((i) => i.action.context.description)).toEqual(['Not sent'])
  sealed.close()
  // Discarding deletes everything of hers.
  await discardForeign()
  expect(runtimeState().foreign).toBeNull()
  expect((await indexedDB.databases()).map((x) => x.name)).not.toContain('taskto-offline-1')
  // Nothing foreign, nothing asked: a person with nothing unsent leaves nothing behind.
  await activate(3, 'Carol')
  expect(runtimeState().foreign).toBeNull()
  expect((await indexedDB.databases()).map((x) => x.name)).not.toContain('taskto-offline-2')
})

test('signing out deletes the account’s database even when it was never opened here', async () => {
  const store = await openStore(7)
  await queue(store, { description: 'x', amount: '1' })
  store.close()
  window.localStorage.setItem('taskto:offline-users', '[7]')
  rememberUser({ id: 7, name: 'Pat' })
  expect(runtimeState().runtime).toBeNull()
  await deactivate({ purge: true, user: 7 })
  expect((await indexedDB.databases()).map((x) => x.name)).not.toContain('taskto-offline-7')
  expect(lastUser()).toBeNull()
})

const narrowed = { id: 1, name: 'Expenses', access: { read_records: 'own' } }

test('revalidating a copy of all the records, when the person now reads only their own, downloads it again', async () => {
  const store = await openStore()
  await store.putDownload(download(7, { scopeOf: 'all', records: 5, total: 5 }))
  fakeFetch({
    'GET /api/workspaces/1/spaces/2': { body: { id: 2, offline_downloads: true } },
    [`GET ${base}`]: { body: narrowed },
    [`GET ${base}/offline`]: { body: offlineApp({ scope: 'own', records: 1, total: 1 }) },
  })
  expect(await revalidate(store)).toEqual([])
  expect(await store.getDownload(scope)).toMatchObject({ scopeOf: 'own', records: 1 })
  // If the new copy can't be had, the old one is not kept: it holds what they may not read.
  await store.putDownload(download(7, { scopeOf: 'all', records: 5, total: 5 }))
  fakeFetch({
    'GET /api/workspaces/1/spaces/2': { body: { id: 2, offline_downloads: true } },
    [`GET ${base}`]: { body: narrowed },
    [`GET ${base}/offline`]: { status: 503, body: { error: 'busy' } },
  })
  expect((await revalidate(store)).map((d) => d.key)).toEqual([scopeKey(scope)])
  expect(await store.listDownloads()).toEqual([])
})

test('the same person back online after an offline start: downloads an admin turned off are removed', async () => {
  // The app opens with no server: the copy is shown, and nothing could be asked.
  await activate(7, 'Pat')
  const runtime = runtimeState().runtime
  if (!runtime) throw new Error('no runtime')
  await runtime.store.putDownload(download(7))
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => Promise.reject(new TypeError('offline'))),
  )
  await revalidate(runtime.store)
  expect(await runtime.store.listDownloads()).toHaveLength(1)
  // The server is back (the page signs in the same person again), and the admin had turned it off.
  fakeFetch({ 'GET /api/workspaces/1/spaces/2': { body: { id: 2, offline_downloads: false } } })
  await activate(7, 'Pat')
  await waitFor(async () => expect(await runtime.store.listDownloads()).toEqual([]))
})

test('an item added while a pass runs is sent by it, not left queued', async () => {
  const store = await openStore()
  await queue(store, { description: 'First', amount: '1' })
  let added = false
  const engine = engineOf(store)
  fakeFetch({
    [`POST ${base}/actions`]: async () => {
      if (!added) {
        added = true
        // The person adds another record while the first is being sent.
        await queue(store, { description: 'Second', amount: '2' })
        void engine.run()
      }
      return { body: [] }
    },
  })
  await engine.run()
  expect((await store.listItems()).map((i) => i.state)).toEqual(['synced', 'synced'])
})

test('a tab that finds another one sending looks again soon, and sends its item once the lock is free', async () => {
  const store = await openStore()
  await queue(store, { description: 'Mine', amount: '1' })
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  let held = true
  Object.defineProperty(navigator, 'locks', {
    configurable: true,
    value: { request: async (_n: string, _o: unknown, cb: (lock: unknown) => Promise<void>) => cb(held ? null : {}) },
  })
  const timers: { fn: () => void; ms: number }[] = []
  const c = clock()
  const engine = new SyncEngine(store, {
    now: c.now,
    setTimer: (fn, ms) => timers.push({ fn, ms }),
    clearTimer: () => {},
  })
  try {
    await engine.run()
    expect(calls).toHaveLength(0)
    expect(timers.map((t) => t.ms)).toEqual([LOCK_RETRY_MS])
    held = false
    timers[0].fn()
    await waitFor(async () => expect((await store.listItems())[0].state).toBe('synced'))
    expect(calls).toHaveLength(1)
  } finally {
    Reflect.deleteProperty(navigator, 'locks')
  }
})

test('two tabs without Web Locks send a record with a photo once: it stays synced, never needs attention', async () => {
  const a = await openStore()
  const d = download(7, {
    definition: { ...definition, collection: { fields: [{ name: 'photos', type: 'attachment' } as never] } },
  })
  await a.putDownload(d)
  const transport = offlineTransport(idle(a), d)
  await transport.act(press('add_expense', { photos: [transport.photos.stage(blob('x', 'image/png'))] }))
  const b = await openStore()
  // The server: an operation answers its first request again, and refuses another one under it.
  const seen = new Map<string, string>()
  let posts = 0
  fakeFetch({
    [`POST ${base}/actions`]: (call) => {
      posts++
      const body = call.body as { operation: string; context: unknown }
      const first = seen.get(body.operation)
      seen.set(body.operation, first ?? JSON.stringify(body.context))
      return first && first !== JSON.stringify(body.context)
        ? { status: 409, body: { error: 'reused', code: 'app_operation_reused' } }
        : { body: [] }
    },
  })
  let uploads = 0
  const upload = vi.fn(async (_base: string, _field: string, file: Blob) => {
    const n = ++uploads
    await new Promise((resolve) => setTimeout(resolve, 20))
    return {
      id: String(n).padStart(32, '0'),
      field: 'photos',
      status: 'ready' as const,
      size: file.size,
      received: file.size,
      attached: false,
    }
  })
  await Promise.all([engineOf(a, clock(), { upload }).run(), engineOf(b, clock(), { upload }).run()])
  const [item] = await a.listItems()
  expect(item).toMatchObject({ state: 'synced' })
  expect(item.attention).toBeUndefined()
  expect(uploads).toBe(1)
  expect(posts).toBe(1)
})

test('a download that holds a record the device sent is not counted twice, and drops the device’s synced entry', async () => {
  const store = await openStore()
  const d = download(7)
  await store.putDownload(d)
  await queue(store, { description: 'Taxi', amount: '8' }, d)
  await queue(store, { description: 'Bus', amount: '2' }, d)
  const [taxi, bus] = await store.listItems()
  await store.updateItem(taxi.id, (i) => ({ ...i, state: 'synced' }))
  const rows = (dl: Download) => itemsOf(dl, [taxi, bus]).length
  expect(rows(d)).toBe(1 + 2)
  // Downloaded again after the server had the taxi: it is among the server's records, once.
  const withTaxi = download(7, { operations: { [taxi.operation]: 41 } })
  expect(rows(withTaxi)).toBe(1 + 1)
  fakeFetch({
    [`GET ${base}/offline`]: { body: { ...offlineApp(), operations: { [taxi.operation]: 41 } } },
  })
  await downloadApp(store, scope)
  expect((await store.listItems()).map((i) => i.id)).toEqual([bus.id])
})
