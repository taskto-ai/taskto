// @vitest-environment jsdom
//
// Offline edits, deletions and conflict recovery (docs/offline.md, "Changing and deleting records"
// and "Conflicts and what doesn't fit"): a change to a downloaded record waits on the device with
// the revision it was based on, is sent as the account that made it, and never overwrites the
// server: a record that moved on is a visible conflict, one deleted meanwhile is never brought
// back by a change, what no longer fits the app is kept for repair, and the person settles each
// one on purpose. The account guards of the insert outbox hold for these items too.
//
// The server here is a small stand-in that keeps what the real one decides (internal/apps,
// Service.act): a record's revision, a change refused as a conflict that shows the record as it
// is, a deleted record that no change brings back, restore, and an operation key made once.

import 'fake-indexeddb/auto'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import type { OfflineApp } from '../api'
import { forgetOperations } from '../apps/operations'
import { setPreferences } from '../i18n'
import { type Call, fakeFetch } from '../test/fakes'
import { type Deleted, findDeleted, keepServer, problemsOf, reapply, restoreAndApply, saveEdit } from './choices'
import { downloadApp } from './download'
import { useOfflineData } from './hooks'
import { itemsOf } from './local'
import { OfflineApp as OfflineAppView } from './OfflineShell'
import { OutboxList } from './OutboxList'
import { deactivate, rememberUser, runtimeState, setSyncDeps, settle, unsentOf } from './runtime'
import {
  approvalOf,
  type Download,
  ItemChanged,
  kindOf,
  MIGRATIONS,
  OfflineStore,
  type OutboxItem,
  SCHEMA_VERSION,
  scopeKey,
  seenOf,
} from './store'
import { SyncEngine } from './sync'
import { offlineTransport } from './transport'

const scope = { workspace: 1, space: 2, app: 1 }
const base = '/api/workspaces/1/spaces/2/apps/1'

const definition: OfflineApp['definition'] = {
  definition_version: 1,
  requires: [],
  collection: {
    fields: [
      { name: 'description', type: 'string', required: true, max_length: 200 },
      { name: 'amount', type: 'number', required: true },
      { name: 'kind', type: 'choice', options: ['food', 'travel', 'other'] },
    ],
  },
  actions: {
    add_expense: { kind: 'insert' },
    edit_expense: { kind: 'update' },
    delete_expense: { kind: 'delete' },
  },
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
    modifiable: [1],
    ...patch,
    operations: patch.operations ?? {},
  }
}

const press = (name: string, context: Record<string, unknown>, expectedRevision?: number) => ({
  name,
  surfaceId: 'app-1',
  sourceComponentId: 'edit',
  timestamp: '2026-11-02T10:00:00.000Z',
  context,
  ...(expectedRevision === undefined ? {} : { expectedRevision }),
})

beforeEach(async () => {
  globalThis.indexedDB = new IDBFactory()
  window.localStorage.clear()
  rememberUser({ id: 7, name: 'Pat' })
  forgetOperations()
  await setPreferences({})
})

afterEach(async () => {
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  setSyncDeps({})
  await deactivate({ purge: false })
})

function clock() {
  let at = 1_000_000
  return {
    now: () => at,
    advance: (ms: number) => {
      at += ms
    },
  }
}

const engineOf = (store: OfflineStore, c = clock(), extra: ConstructorParameters<typeof SyncEngine>[1] = {}) =>
  new SyncEngine(store, { now: c.now, setTimer: () => 0, clearTimer: () => {}, random: () => 0.5, ...extra })

// The page's engine does not send by itself here (the screen's button asks it to run, and the
// tests decide when); `gate` lets what the person chooses (which asks it to run) be sent.
class GatedEngine extends SyncEngine {
  gate = false
  override run(): Promise<void> {
    return this.gate ? super.run() : Promise.resolve()
  }
}

function runtimeOf(store: OfflineStore) {
  const engine = new GatedEngine(store, {
    now: clock().now,
    setTimer: () => 0,
    clearTimer: () => {},
    random: () => 0.5,
  })
  return { user: store.user, store, engine, stop: () => {} }
}

async function setup(patch: Partial<Download> = {}) {
  const store = await OfflineStore.open(7)
  const d = download(7, patch)
  await store.putDownload(d)
  const runtime = runtimeOf(store)
  return { store, d, runtime, transport: offlineTransport(runtime, d) }
}

type Row = { rev: number; data: Record<string, unknown>; deleted?: boolean; hidden?: boolean }

// What the server decides about a record's revision, its deletion and its operations.
function fakeServer(rows = new Map<number, Row>([[1, { rev: 1, data: { description: 'Coffee', amount: 4.5 } }]])) {
  const ops = new Map<string, string>()
  const itemOf = (id: number) => ({ id, _revision: rows.get(id)?.rev, ...rows.get(id)?.data })
  const answer = () => [
    {
      updateDataModel: {
        path: '/',
        value: { items: [...rows.entries()].filter(([, r]) => !r.deleted).map(([id]) => itemOf(id)) },
      },
    },
  ]
  const calls = fakeFetch({
    [`POST ${base}/actions`]: (call) => {
      const b = call.body as {
        name: string
        context: { id: number } & Record<string, unknown>
        expectedRevision?: number
        operation: string
        release: number
      }
      const key = JSON.stringify([b.name, b.context, b.expectedRevision, b.release])
      if (ops.has(b.operation)) {
        return ops.get(b.operation) === key
          ? { body: answer() }
          : { status: 409, body: { error: 'reused', code: 'app_operation_reused' } }
      }
      const row = rows.get(b.context.id)
      if (!row || row.deleted) {
        return { status: 422, body: { error: 'no row', code: 'app_row_not_found' } }
      }
      if (b.expectedRevision !== undefined && b.expectedRevision !== row.rev) {
        return {
          status: 409,
          body: {
            error: 'changed',
            code: 'app_record_conflict',
            params: { id: b.context.id },
            current: itemOf(b.context.id),
          },
        }
      }
      if (b.name === 'delete_expense') row.deleted = true
      else {
        const { id: _id, ...fields } = b.context
        for (const [k, v] of Object.entries(fields)) row.data[k] = k === 'amount' ? Number(v) : v
      }
      row.rev++
      ops.set(b.operation, key)
      return { body: answer() }
    },
    [`GET ${base}/records/deleted`]: () => ({
      body: {
        records: [...rows.entries()]
          .filter(([, r]) => r.deleted)
          .map(([id, r]) => ({
            id,
            revision: r.rev,
            data: r.data,
            deleted_at: '2026-11-03T09:00:00.000Z',
            deleted_by: { id: 2, name: 'Sam' },
          })),
      },
    }),
    // A record as it is now, for whoever may read it (the record's history, whose `record` it is).
    [`GET ${base}/records/1/history`]: () => {
      const row = rows.get(1) as Row
      if (row.hidden) return { status: 404, body: { error: 'no record', code: 'app_row_not_found' } }
      return {
        body: {
          record: {
            id: 1,
            revision: row.rev,
            data: row.data,
            deleted_at: row.deleted ? '2026-11-03T09:00:00.000Z' : null,
          },
          history: [],
          truncated: false,
        },
      }
    },
    [`POST ${base}/records/1/restore`]: (call) => {
      const row = rows.get(1) as Row
      const at = (call.body as { expected_revision: number }).expected_revision
      if (!row.deleted || row.rev !== at) {
        return {
          status: 409,
          body: {
            error: 'changed',
            code: 'app_record_conflict',
            params: { id: 1 },
            ...(row.deleted ? {} : { current: itemOf(1) }),
          },
        }
      }
      row.deleted = false
      row.rev++
      return { body: { id: 1, revision: row.rev, data: row.data } }
    },
  })
  return { rows, calls, ops, itemOf }
}

const actionCalls = (calls: Call[]) => calls.filter((c) => c.method === 'POST' && c.path === `${base}/actions`)

// --- the queue: what a change is, and how several to one record are held

test('an edit and a deletion wait on the device with the record, the revision they were based on, the release and a stable key', async () => {
  const { store, d, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const [edit] = await store.listItems()
  expect(edit).toMatchObject({
    kind: 'update',
    state: 'queued',
    user: 7,
    release: 3,
    schema: 1,
    record: { id: 1, revision: 1 },
    original: { description: 'Coffee', amount: 4.5 },
    action: { name: 'edit_expense', context: { id: 1, amount: '9' } },
  })
  expect(edit.operation).toMatch(/^[A-Za-z0-9_-]{8,64}$/)
  // Shown as the person left it, and said to be waiting: its revision is the one it is based on.
  expect(itemsOf(d, [edit])).toEqual([{ id: 1, _revision: 1, _pending: 'update', description: 'Coffee', amount: 9 }])
  // It survives a restart.
  store.close()
  const again = await OfflineStore.open(7)
  expect((await again.listItems())[0]).toMatchObject({
    id: edit.id,
    operation: edit.operation,
    record: { id: 1, revision: 1 },
  })
})

test('a change needs the revision the person saw, a record the server said may be changed, and one that is on the server', async () => {
  const { store, d, runtime } = await setup()
  const transport = offlineTransport(runtime, d)
  await expect(transport.act(press('edit_expense', { id: 1, amount: '9' }))).rejects.toMatchObject({
    code: 'offline_edit_unavailable',
  })
  await expect(transport.act(press('edit_expense', { id: 2, amount: '9' }, 1))).rejects.toMatchObject({
    code: 'app_forbidden',
  })
  await expect(transport.act(press('edit_expense', { id: -5, amount: '9' }, 1))).rejects.toMatchObject({
    code: 'offline_record_unsynced',
  })
  // The app's own rules are applied here too: a required field can't be emptied.
  await expect(transport.act(press('edit_expense', { id: 1, amount: '' }, 1))).rejects.toMatchObject({
    code: 'app_field_required',
  })
  await expect(transport.act(press('edit_expense', { id: 1, amount: 'abc' }, 1))).rejects.toMatchObject({
    code: 'app_field_not_number',
  })
  expect(await store.listItems()).toEqual([])
  // A copy made by an earlier version, which didn't say which records may change, offers no change.
  await store.putDownload(download(7, { modifiable: undefined }))
  await expect(transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))).rejects.toMatchObject({
    code: 'offline_edit_unavailable',
  })
})

test('several edits of one action to one record are one item while none was sent: the later values win; a deletion is an item of its own', async () => {
  const { store, d, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const [first] = await store.listItems()
  await transport.act(press('edit_expense', { id: 1, description: 'Espresso', amount: '10' }, 1))
  let items = await store.listItems()
  expect(items).toHaveLength(1)
  expect(items[0]).toMatchObject({
    id: first.id,
    seq: first.seq,
    kind: 'update',
    record: { id: 1, revision: 1 },
    action: { context: { id: 1, description: 'Espresso', amount: '10' } },
  })
  // The request changed, so it is made under a new key (the old one was never sent).
  expect(items[0].operation).not.toBe(first.operation)
  expect(itemsOf(d, items)[0]).toMatchObject({ description: 'Espresso', amount: 10, _pending: 'update' })
  // A deletion is another action, with an identity of its own (what it sets going is the server's
  // to know): it is queued after the edit, on the same revision, and replaces nothing.
  await transport.act(press('delete_expense', { id: 1 }, 1))
  items = await store.listItems()
  expect(items).toHaveLength(2)
  expect(items[0]).toMatchObject({ id: first.id, kind: 'update', action: { name: 'edit_expense' } })
  expect(items[1]).toMatchObject({
    kind: 'delete',
    record: { id: 1, revision: 1 },
    action: { name: 'delete_expense', context: { id: 1 } },
  })
  // What the person saw when they deleted it: the record with their edit.
  expect(items[1].original).toEqual({ description: 'Espresso', amount: 10 })
  expect(itemsOf(d, items)).toEqual([])
})

test('once a send was started and not answered for good, the record takes no further change until it is known', async () => {
  const { store, runtime, d } = await setup()
  const transport = offlineTransport(runtime, d)
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  fakeFetch({ [`POST ${base}/actions`]: { status: 503, body: { error: 'busy' } } })
  const c = clock()
  await engineOf(store, c).run()
  const [item] = await store.listItems()
  expect(item).toMatchObject({ state: 'queued', attempts: 1, unconfirmed: true })
  await expect(transport.act(press('edit_expense', { id: 1, amount: '11' }, 1))).rejects.toMatchObject({
    code: 'offline_record_busy',
  })
  await expect(transport.act(press('delete_expense', { id: 1 }, 1))).rejects.toMatchObject({
    code: 'offline_record_busy',
  })
  // Nothing was changed, and the same request is still the one that will be sent.
  expect(await store.listItems()).toEqual([item])
  // Once the server has answered, the record is free again, and a later change is based on the
  // revision the server gave.
  const server = fakeServer()
  c.advance(10 * 60_000)
  await engineOf(store, c).run()
  const [synced] = await store.listItems()
  expect(synced).toMatchObject({ state: 'synced', ack: { id: 1, _revision: 2, description: 'Coffee', amount: 9 } })
  expect(synced.unconfirmed).toBeUndefined()
  expect(server.rows.get(1)?.rev).toBe(2)
  expect(itemsOf(d, [synced])[0]).toMatchObject({ amount: 9, _revision: 2 })
  const later = offlineTransport(runtime, d)
  await later.act(press('edit_expense', { id: 1, amount: '12' }, 2))
  expect((await store.listItems()).at(-1)).toMatchObject({ kind: 'update', state: 'queued', record: { revision: 2 } })
})

// --- replay: one send, the revision it is based on, and the answers

test('an edit is sent with the revision it was based on, its release and its key, as the account that made it, and is made once', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const server = fakeServer()
  const engine = engineOf(store)
  await engine.run()
  const [item] = await store.listItems()
  expect(item).toMatchObject({ state: 'synced', ack: { id: 1, _revision: 2 } })
  const [call] = actionCalls(server.calls)
  expect(call.body).toMatchObject({
    name: 'edit_expense',
    context: { id: 1, amount: '9' },
    expectedRevision: 1,
    release: 3,
    operation: item.operation,
  })
  expect(call.headers['x-taskto-expected-user']).toBe('7')
  expect(server.rows.get(1)?.data.amount).toBe(9)
  // Sent again (the answer was lost, another tab), it is the same request and changes nothing.
  await store.updateItem(item.id, (i) => ({ ...i, state: 'queued' }))
  await engine.run()
  expect(server.rows.get(1)?.rev).toBe(2)
  expect(runtime.user).toBe(7)
})

test('two devices editing one record: the second is a visible conflict that overwrites nothing, and its input is kept', async () => {
  const { store, d, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  // The other device got there first, from the same revision.
  const server = fakeServer()
  const row = server.rows.get(1) as Row
  row.data.description = 'Flat white'
  row.rev = 2
  const engine = engineOf(store)
  await engine.run()
  const [item] = await store.listItems()
  expect(item).toMatchObject({
    state: 'attention',
    attention: 'conflict',
    kind: 'update',
    attempts: 0,
    conflict: { current: { id: 1, _revision: 2, description: 'Flat white', amount: 4.5 } },
    action: { context: { id: 1, amount: '9' } },
  })
  expect(item.unconfirmed).toBeUndefined()
  // Nothing was overwritten, and the person's change is still shown as theirs, as pending.
  expect(row.data).toEqual({ description: 'Flat white', amount: 4.5 })
  expect(itemsOf(d, [item])[0]).toMatchObject({ amount: 9, _pending: 'update' })
  // Not retried by itself, by "Sync now" or by a later pass: it is the person's to settle.
  await engine.retry()
  await engine.run()
  expect(actionCalls(server.calls)).toHaveLength(1)
})

test('applying my change on top sends the same change on the revision just shown, under a new key, and the server keeps the other device’s change', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const server = fakeServer()
  const row = server.rows.get(1) as Row
  row.data.description = 'Flat white'
  row.rev = 2
  await engineOf(store).run()
  const [conflicted] = await store.listItems()
  runtime.engine.gate = true
  expect(await reapply(runtime, conflicted)).toBe(true)
  await waitFor(async () => expect((await store.listItems())[0].state).toBe('synced'))
  const [item] = await store.listItems()
  expect(item.operation).not.toBe(conflicted.operation)
  const [, second] = actionCalls(server.calls)
  expect(second.body).toMatchObject({ expectedRevision: 2, context: { id: 1, amount: '9' }, operation: item.operation })
  expect(row).toMatchObject({ rev: 3, data: { description: 'Flat white', amount: 9 } })
  // The copy on the device holds the record as the server had it when the choice was made.
  expect(item.conflict).toBeUndefined()
})

test('keeping the server’s version discards my change and the copy holds the record as the server has it', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const server = fakeServer()
  ;(server.rows.get(1) as Row).data.description = 'Flat white'
  ;(server.rows.get(1) as Row).rev = 2
  await engineOf(store).run()
  const [conflicted] = await store.listItems()
  expect(await keepServer(runtime, conflicted)).toBe(true)
  expect(await store.listItems()).toEqual([])
  const copy = (await store.getDownload(scope)) as Download
  expect(itemsOf(copy, [])).toEqual([{ id: 1, _revision: 2, description: 'Flat white', amount: 4.5 }])
  expect(copy.records).toBe(1)
})

test('editing and trying again sends only what differs from the server’s record, on its revision', async () => {
  const { store, d, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const server = fakeServer()
  ;(server.rows.get(1) as Row).data.description = 'Flat white'
  ;(server.rows.get(1) as Row).rev = 2
  await engineOf(store).run()
  const [conflicted] = await store.listItems()
  // The form starts from the server's record with the person's change on top; they change the
  // choice and leave the rest.
  runtime.engine.gate = true
  expect(await saveEdit(runtime, conflicted, { description: 'Flat white', amount: '9', kind: 'food' }, d)).toBe(true)
  await waitFor(async () => expect((await store.listItems())[0].state).toBe('synced'))
  expect(actionCalls(server.calls)[1].body).toMatchObject({
    expectedRevision: 2,
    context: { id: 1, amount: '9', kind: 'food' },
  })
  expect(actionCalls(server.calls)[1].body).not.toHaveProperty('context.description')
  expect(server.rows.get(1)?.data.description).toBe('Flat white')
})

test('a stale choice is not applied: only an item that still needs attention, as it was shown', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const server = fakeServer()
  ;(server.rows.get(1) as Row).rev = 2
  await engineOf(store).run()
  const [conflicted] = await store.listItems()
  // Another tab settled it meanwhile (a new request, under a new key).
  runtime.engine.gate = true
  expect(await reapply(runtime, conflicted)).toBe(true)
  // What was chosen on the item as it was shown is refused for an item that has moved on, and
  // says why: the person reviews it again.
  await expect(keepServer(runtime, conflicted)).rejects.toMatchObject({ code: 'offline_item_changed' })
  runtime.engine.gate = true
  await expect(reapply(runtime, conflicted)).rejects.toMatchObject({ code: 'offline_item_changed' })
  expect((await store.listItems())[0].operation).not.toBe(conflicted.operation)
})

test('an edit of a record deleted on the server is a visible conflict, never a resurrection; restoring is the person’s own step', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const server = fakeServer()
  const row = server.rows.get(1) as Row
  row.deleted = true
  row.rev = 2
  const engine = engineOf(store)
  await engine.run()
  const [item] = await store.listItems()
  expect(item).toMatchObject({ state: 'attention', attention: 'deleted', kind: 'update' })
  expect(item.action.context).toEqual({ id: 1, amount: '9' })
  // Nothing asked for a restore, and the record is still deleted.
  expect(server.calls.some((c) => c.path.endsWith('/restore'))).toBe(false)
  expect(row.deleted).toBe(true)
  await engine.run()
  expect(actionCalls(server.calls)).toHaveLength(1)

  // The person chooses to restore it and apply their change: the restore is made first, and the
  // change goes on the revision the restore gave.
  const { findDeleted } = await import('./choices')
  const deleted = await findDeleted(runtime, item)
  expect(deleted).toEqual({ revision: 2, at: '2026-11-03T09:00:00.000Z', by: 'Sam' })
  runtime.engine.gate = true
  expect(await restoreAndApply(runtime, item, deleted as NonNullable<typeof deleted>)).toBe(true)
  await waitFor(async () => expect((await store.listItems())[0].state).toBe('synced'))
  const paths = server.calls.filter((c) => c.method === 'POST').map((c) => c.path)
  expect(paths.slice(-2)).toEqual([`${base}/records/1/restore`, `${base}/actions`])
  expect(server.calls.find((c) => c.path.endsWith('/restore'))?.body).toEqual({ expected_revision: 2 })
  expect(actionCalls(server.calls)[1].body).toMatchObject({ expectedRevision: 3, context: { id: 1, amount: '9' } })
  expect(row).toMatchObject({ deleted: false, rev: 4, data: { amount: 9 } })
})

test('a restore whose answer was lost is the same restore: the change goes on from it, and nothing is restored twice', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const server = fakeServer()
  const row = server.rows.get(1) as Row
  row.deleted = true
  row.rev = 2
  await engineOf(store).run()
  const [item] = await store.listItems()
  // The person chooses to restore, and the restore is made, but its answer never comes.
  await restoreAndApply(runtime, item, { revision: 2, at: null, by: null })
  row.deleted = false
  row.rev = 3
  await engineOf(store).run()
  expect((await store.listItems())[0].state).toBe('synced')
  expect(row).toMatchObject({ rev: 4, data: { amount: 9 } })
})

test('a deletion of a record that changed on the server is a conflict; deleting anyway is a choice made on what was shown', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('delete_expense', { id: 1 }, 1))
  const server = fakeServer()
  const row = server.rows.get(1) as Row
  row.data.amount = 20
  row.rev = 2
  await engineOf(store).run()
  const [item] = await store.listItems()
  expect(item).toMatchObject({
    kind: 'delete',
    state: 'attention',
    attention: 'conflict',
    conflict: { current: { amount: 20 } },
  })
  expect(row.deleted).toBeUndefined()
  runtime.engine.gate = true
  expect(await reapply(runtime, item)).toBe(true)
  await waitFor(async () => expect((await store.listItems())[0].state).toBe('synced'))
  expect(actionCalls(server.calls)[1].body).toMatchObject({ name: 'delete_expense', expectedRevision: 2 })
  expect(row.deleted).toBe(true)
})

test('a deletion of a record already deleted has nothing left to send: it says so, and is cleared by the person', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('delete_expense', { id: 1 }, 1))
  const server = fakeServer()
  const row = server.rows.get(1) as Row
  row.deleted = true
  row.rev = 2
  await engineOf(store).run()
  const [item] = await store.listItems()
  expect(item).toMatchObject({ kind: 'delete', attention: 'deleted' })
  expect(await keepServer(runtime, item)).toBe(true)
  expect(await store.listItems()).toEqual([])
  // The copy no longer holds the record the server deleted.
  expect(itemsOf((await store.getDownload(scope)) as Download, [])).toEqual([])
})

test('a download taken after another device deleted a record drops it from the copy; a change waiting for it stays, to be settled', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const empty = surface.map((m) =>
    m.updateDataModel
      ? { ...m, updateDataModel: { ...m.updateDataModel, value: { draft: {}, items: [], total: '0.00' } } }
      : m,
  )
  fakeFetch({
    [`GET ${base}/offline`]: {
      body: { ...offlineApp(), messages: empty, records: 0, total: 0, modifiable: [] },
    },
  })
  const copy = await downloadApp(store, scope)
  expect(itemsOf(copy, [])).toEqual([])
  expect(await store.listItems()).toHaveLength(1)
  expect(runtime.user).toBe(7)
  // A synced change to a record the download no longer holds goes with it.
  const [item] = await store.listItems()
  await store.updateItem(item.id, (i) => ({ ...i, state: 'synced' }))
  await downloadApp(store, scope)
  expect(await store.listItems()).toEqual([])
})

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
    captured_at: '2026-11-04T10:00:00.000Z',
    modifiable: [1],
    ...patch,
  }
}

// --- what doesn't fit the app any more

test('a field that no longer fits is kept for repair: what doesn’t fit is shown, and the person fixes it against the app as it is now', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9', kind: 'food' }, 1))
  // The app changed: the choice no longer has "food".
  fakeFetch({
    [`POST ${base}/actions`]: {
      status: 422,
      body: { error: 'not an option', code: 'app_field_not_option', params: { field: 'kind' } },
    },
  })
  await engineOf(store).run()
  const [item] = await store.listItems()
  expect(item).toMatchObject({
    state: 'attention',
    attention: 'schema',
    problems: [{ field: 'kind', code: 'app_field_not_option' }],
    action: { context: { id: 1, amount: '9', kind: 'food' } },
  })
  const live: OfflineApp['definition'] = {
    ...definition,
    definition_version: 2,
    collection: {
      fields: [
        { name: 'description', type: 'string', required: true },
        { name: 'amount', type: 'number', required: true },
        { name: 'kind', type: 'choice', options: ['meal', 'travel'] },
        { name: 'note', type: 'string', required: true },
      ],
    },
  }
  // What doesn't fit, as the device finds it against the live app: a value no longer an option,
  // and a field that is required now (the record has none).
  expect(problemsOf(live, 'update', item.action.context, item.original ?? {}).map((p) => [p.field, p.code])).toEqual([
    ['kind', 'app_field_not_option'],
    ['note', 'app_field_required'],
  ])
  // A field the app no longer has, and one that was retyped.
  const gone = { ...live, collection: { fields: [{ name: 'amount', type: 'boolean' as const }] } }
  expect(
    problemsOf(gone as OfflineApp['definition'], 'insert', { amount: '9', kind: 'food' }).map((p) => [p.field, p.code]),
  ).toEqual([
    ['amount', 'bad_request'],
    ['kind', 'offline_field_removed'],
  ])
  // Repaired: the item is made for the live release, on a new key, and sent.
  const server = fakeServer()
  ;(server.rows.get(1) as Row).data.note = 'x'
  const fixed = download(7, { release: 4, definition: live })
  await store.putDownload(fixed)
  runtime.engine.gate = true
  expect(await saveEdit(runtime, item, { description: 'Coffee', amount: '9', kind: 'meal', note: 'x' }, fixed)).toBe(
    true,
  )
  await waitFor(async () => expect((await store.listItems())[0].state).toBe('synced'))
  expect(actionCalls(server.calls)[0].body).toMatchObject({
    release: 4,
    expectedRevision: 1,
    context: { id: 1, amount: '9', kind: 'meal', note: 'x' },
  })
  expect((await store.listItems())[0].schema).toBe(2)
})

test('values that don’t fit the app the person is repairing against are refused with the server’s words, and nothing is saved', async () => {
  const { store, d, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9', kind: 'food' }, 1))
  fakeFetch({
    [`POST ${base}/actions`]: {
      status: 422,
      body: { error: 'x', code: 'app_field_not_option', params: { field: 'kind' } },
    },
  })
  await engineOf(store).run()
  const [item] = await store.listItems()
  await expect(saveEdit(runtime, item, { description: 'Coffee', amount: '9', kind: 'nope' }, d)).rejects.toMatchObject({
    code: 'app_field_not_option',
  })
  await expect(saveEdit(runtime, item, { description: 'Coffee', amount: '4.5', kind: '' }, d)).rejects.toMatchObject({
    code: 'offline_nothing_to_change',
  })
  expect(await store.listItems()).toEqual([item])
})

// --- permanent failures, and access

test('a refusal for good, lost access, an archived app: needs attention, keeps the input and is never retried by itself', async () => {
  const { store, transport } = await setup()
  await transport.act(press('delete_expense', { id: 1 }, 1))
  const calls = fakeFetch({
    [`POST ${base}/actions`]: {
      status: 403,
      body: { error: 'no', code: 'app_forbidden', params: { capability: 'modify_records' } },
    },
    [`GET ${base}`]: { body: { id: 1 } },
  })
  const engine = engineOf(store)
  await engine.run()
  let [item] = await store.listItems()
  expect(item).toMatchObject({ kind: 'delete', state: 'attention', attention: 'access' })
  expect(item.unconfirmed).toBeUndefined()
  const sent = calls.length
  await engine.run()
  await engine.run()
  expect(calls).toHaveLength(sent)
  // Archived.
  fakeFetch({ [`POST ${base}/actions`]: { status: 409, body: { error: 'archived', code: 'app_offline' } } })
  await engine.retry(item.id)
  ;[item] = await store.listItems()
  expect(item).toMatchObject({ state: 'attention', attention: 'archived' })
  // Revoked: replay stops and the copy goes, the input stays.
  fakeFetch({
    [`POST ${base}/actions`]: { status: 404, body: { error: 'nf', code: 'not_found' } },
    [`GET ${base}`]: { status: 404, body: { error: 'nf', code: 'not_found' } },
  })
  const revoked = vi.fn(async () => store.removeDownload(scope))
  await engineOf(store, clock(), { onRevoked: revoked }).retry(item.id)
  expect(revoked).toHaveBeenCalled()
  expect(await store.listDownloads()).toEqual([])
  ;[item] = await store.listItems()
  expect(item).toMatchObject({ state: 'attention', attention: 'access', action: { context: { id: 1 } } })
})

test('an older release that can no longer take the change keeps it, for repair', async () => {
  const { store, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  fakeFetch({
    [`POST ${base}/actions`]: {
      status: 409,
      body: { error: 'old', code: 'app_release_outdated', params: { release: 3, live: 4 } },
    },
  })
  await engineOf(store).run()
  expect((await store.listItems())[0]).toMatchObject({
    state: 'attention',
    attention: 'outdated',
    record: { revision: 1 },
  })
})

// --- accounts

test('a change is sent only as the account that made it: another account signed in pauses it, and nothing is counted against it', async () => {
  const { store, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const server = fakeServer()
  const engine = engineOf(store, clock(), { signedIn: () => 8 })
  await engine.run()
  expect(server.calls).toHaveLength(0)
  expect(engine.status().paused).toBe('account')
  expect((await store.listItems())[0]).toMatchObject({ state: 'queued', attempts: 0 })
  // The server refusing the session as another's, mid-flight, is not the change's failure either.
  fakeFetch({ [`POST ${base}/actions`]: { status: 409, body: { error: 'x', code: 'session_account_changed' } } })
  const second = engineOf(store)
  await second.run()
  expect(second.status().paused).toBe('account')
  const [item] = await store.listItems()
  expect(item).toMatchObject({ state: 'queued', attempts: 0 })
  expect(item.unconfirmed).toBeUndefined()
})

test('unsent changes are never lost by signing out or in as someone else: they are counted, and kept sealed', async () => {
  const { store, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  await transport.act(press('add_expense', { description: 'Tea', amount: '2' }))
  expect((await unsentOf(7)).count).toBe(2)
  const left = await store.retireOrKeepUnsent(new Map(), false)
  expect(left).toHaveLength(2)
  expect(await store.listItems()).toHaveLength(2)
  // Only what the person was shown and agreed to lose goes.
  const [change] = (await store.listItems()).filter((i) => kindOf(i) === 'update')
  expect(await store.retireOrKeepUnsent(approvalOf([{ id: change.id, ...seenOf(change) }]), false)).toHaveLength(2)
  expect(await store.listItems()).toHaveLength(2)
  // Sealed for another account: its downloads go, the unsent change stays.
  const kept = await store.retireOrKeepUnsent()
  expect(kept).toHaveLength(2)
  expect(await store.listDownloads()).toEqual([])
  expect((await store.listItems()).map((i) => i.state)).toEqual(['queued', 'queued'])
  expect(runtimeState().runtime).toBeNull()
  expect(await settle(7, new Map())).toHaveLength(2)
})

// --- the database

test('a queue written by the previous version of the database keeps working: its items are migrated as inserts and are sent', async () => {
  expect(SCHEMA_VERSION).toBe(2)
  // The database exactly as version 1 left it, with a record waiting.
  const name = OfflineStore.databaseName(7)
  const v1 = await new Promise<IDBDatabase>((resolve, reject) => {
    const open = indexedDB.open(name, 1)
    open.onupgradeneeded = () => MIGRATIONS[1](open.result, open.transaction as IDBTransaction)
    open.onsuccess = () => resolve(open.result)
    open.onerror = () => reject(open.error)
  })
  const legacy = {
    id: 'old-1',
    seq: 1,
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
      context: { description: 'Lunch', amount: '12' },
    },
    operation: 'off-legacy-1',
    record: { id: -5, revision: null },
    attachments: [],
    state: 'queued',
    attempts: 0,
    createdAt: '2026-11-02T10:00:00.000Z',
    updatedAt: '2026-11-02T10:00:00.000Z',
  }
  await new Promise<void>((resolve, reject) => {
    const tx = v1.transaction(['outbox', 'meta', 'downloads'], 'readwrite')
    tx.objectStore('outbox').put(legacy)
    tx.objectStore('meta').put({ key: 'seq', value: 1 })
    tx.objectStore('downloads').put(download(7, { modifiable: undefined }))
    tx.oncomplete = () => resolve()
    tx.onerror = () => reject(tx.error)
  })
  v1.close()
  const store = await OfflineStore.open(7)
  const [item] = await store.listItems()
  expect(item).toMatchObject({
    id: 'old-1',
    kind: 'insert',
    operation: 'off-legacy-1',
    state: 'queued',
    record: { id: -5 },
  })
  // The copy it was drawn from is still there, and still offers no change until downloaded again.
  expect((await store.getDownload(scope))?.modifiable).toBeUndefined()
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  await engineOf(store).run()
  expect(actionCalls(calls)).toHaveLength(1)
  expect(actionCalls(calls)[0].body).toMatchObject({
    operation: 'off-legacy-1',
    release: 3,
    context: { description: 'Lunch' },
  })
  expect(actionCalls(calls)[0].body).not.toHaveProperty('expectedRevision')
  expect((await store.listItems())[0].state).toBe('synced')
})

// --- the screen

function Harness({ runtime }: { runtime: ReturnType<typeof runtimeOf> }) {
  const data = useOfflineData(runtime)
  return (
    <OutboxList
      runtime={runtime}
      items={data.items}
      status={data.status}
      apps={{ [scopeKey(scope)]: 'Expenses' }}
      downloads={data.downloads}
    />
  )
}

test('the screen: a deleted record leaves the list as pending, and a conflict shows my change beside the server’s record, with each choice', async () => {
  const { store, d, runtime } = await setup()
  const items: OutboxItem[] = []
  render(<OfflineAppView runtime={runtime} download={d} items={items} />)
  await screen.findByText('Coffee')
  fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
  await waitFor(() => expect(screen.queryByText('Coffee')).toBeNull())
  const [item] = await store.listItems()
  expect(item).toMatchObject({ kind: 'delete', record: { id: 1, revision: 1 }, state: 'queued' })
  cleanup()

  // Another device changed the record meanwhile.
  const server = fakeServer()
  const row = server.rows.get(1) as Row
  row.data.amount = 20
  row.rev = 2
  runtime.engine.gate = true
  await runtime.engine.run()
  render(<Harness runtime={runtime} />)
  const entry = await screen.findByText(/Expenses: delete Coffee/)
  const list = entry.closest('li') as HTMLElement
  expect(within(list).getByRole('alert').textContent).toMatch(/changed on the server/)
  // Not sent again by "Try again": it needs a choice.
  expect(within(list).queryByRole('button', { name: /Try .* again now/ })).toBeNull()
  fireEvent.click(within(list).getByRole('button', { name: /Review/ }))
  const review = await within(list).findByRole('region', { name: /Review/ })
  // What the person saw, what they did, and what the server has.
  expect(within(review).getByText('When you started')).toBeTruthy()
  expect(within(review).getByText('On the server now')).toBeTruthy()
  expect(within(review).getAllByText('(deleted)').length).toBeGreaterThan(0)
  expect(within(review).getByText('20')).toBeTruthy()
  expect(within(review).getByRole('button', { name: 'Delete it anyway' })).toBeTruthy()
  // Keeping the server's version discards the deletion and the record is back.
  fireEvent.click(within(review).getByRole('button', { name: 'Keep the server’s version' }))
  await waitFor(async () => expect(await store.listItems()).toEqual([]))
  expect(row.deleted).toBeUndefined()
  const copy = (await store.getDownload(scope)) as Download
  expect(itemsOf(copy, [])[0]).toMatchObject({ id: 1, _revision: 2, amount: 20 })
})

test('the screen: a record deleted meanwhile says so, offers to restore it and apply the change, or to discard it', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const server = fakeServer()
  const row = server.rows.get(1) as Row
  row.deleted = true
  row.rev = 2
  runtime.engine.gate = true
  await runtime.engine.run()
  render(<Harness runtime={runtime} />)
  const list = (await screen.findByText(/Expenses: change to Coffee/)).closest('li') as HTMLElement
  fireEvent.click(within(list).getByRole('button', { name: /Review/ }))
  const review = await within(list).findByRole('region', { name: /Review/ })
  expect(within(review).getByText(/deleted on the server/i)).toBeTruthy()
  // Who deleted it and when comes from the server's list of deleted records.
  expect(await within(review).findByText(/Deleted by Sam/)).toBeTruthy()
  fireEvent.click(within(review).getByRole('button', { name: 'Restore it and apply my change' }))
  await waitFor(async () => expect((await store.listItems())[0].state).toBe('synced'))
  expect(row).toMatchObject({ deleted: false, rev: 4 })
})

test('the screen: what no longer fits is listed, can be discarded on purpose, and is gone only then', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9', kind: 'food' }, 1))
  fakeFetch({
    [`POST ${base}/actions`]: {
      status: 422,
      body: { error: 'x', code: 'app_field_not_option', params: { field: 'kind' } },
    },
    [`GET ${base}/offline`]: {
      body: offlineApp({
        release: 4,
        definition: {
          ...definition,
          definition_version: 2,
          collection: {
            fields: [
              { name: 'description', type: 'string', required: true },
              { name: 'amount', type: 'number', required: true },
              { name: 'kind', type: 'choice', options: ['meal'] },
            ],
          },
        },
      }),
    },
  })
  runtime.engine.gate = true
  await runtime.engine.run()
  render(<Harness runtime={runtime} />)
  const list = (await screen.findByText(/Expenses: change to Coffee/)).closest('li') as HTMLElement
  expect(within(list).getByRole('alert').textContent).toMatch(/doesn’t fit the app/)
  fireEvent.click(within(list).getByRole('button', { name: /Review/ }))
  const review = await within(list).findByRole('region', { name: /Review/ })
  expect(await within(review).findByText(/no longer fits the app/i)).toBeTruthy()
  // What doesn't fit is named, with the field.
  await waitFor(() => expect(within(review).getByText(/kind must be one of the options/)).toBeTruthy())
  // Discarding asks, and removes the item with its input; nothing is sent.
  fireEvent.click(within(review).getByRole('button', { name: /Discard/ }))
  fireEvent.click(within(review).getByRole('button', { name: 'Yes, discard' }))
  await waitFor(async () => expect(await store.listItems()).toEqual([]))
})

function Shell({ runtime }: { runtime: ReturnType<typeof runtimeOf> }) {
  const data = useOfflineData(runtime)
  const d = data.downloads[0]
  return d ? <OfflineAppView runtime={runtime} download={d} items={data.items} /> : null
}

test('the screen draws a settled record again: the server’s version after a conflict, and the next change is based on its revision', async () => {
  const { store, runtime } = await setup()
  render(<Shell runtime={runtime} />)
  await screen.findByText('Coffee')
  fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
  await waitFor(() => expect(screen.queryByText('Coffee')).toBeNull())
  expect(screen.getByText(/1 change to a record you downloaded is waiting to sync/)).toBeTruthy()
  const server = fakeServer()
  const row = server.rows.get(1) as Row
  row.data.amount = 20
  row.rev = 2
  runtime.engine.gate = true
  await runtime.engine.run()
  await waitFor(async () => expect((await store.listItems())[0]?.attention).toBe('conflict'))
  const [conflicted] = await store.listItems()
  await keepServer(runtime, conflicted)
  // The record is back as the server has it, drawn from the copy the choice changed.
  expect(await screen.findByText('Coffee')).toBeTruthy()
  expect(screen.getByText('20')).toBeTruthy()
  // A change made now is based on the revision the server has.
  runtime.engine.gate = false
  const transport = offlineTransport(runtime, (await store.getDownload(scope)) as Download)
  await transport.act(press('edit_expense', { id: 1, amount: '21' }, 2))
  expect((await store.listItems())[0]).toMatchObject({ kind: 'update', record: { id: 1, revision: 2 } })
})

test('a record added offline that no longer fits the app is repaired the same way: the input is kept, and it is sent for the live release', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('add_expense', { description: 'Taxi', amount: '8', kind: 'food' }))
  fakeFetch({
    [`POST ${base}/actions`]: {
      status: 422,
      body: { error: 'x', code: 'app_field_not_option', params: { field: 'kind' } },
    },
  })
  await engineOf(store).run()
  const [item] = await store.listItems()
  expect(item).toMatchObject({ kind: 'insert', state: 'attention', attention: 'schema' })
  const live = download(7, {
    release: 4,
    definition: {
      ...definition,
      definition_version: 2,
      collection: {
        fields: [
          { name: 'description', type: 'string', required: true },
          { name: 'amount', type: 'number', required: true },
          { name: 'kind', type: 'choice', options: ['meal'] },
        ],
      },
    },
  })
  await store.putDownload(live)
  runtime.engine.gate = true
  await expect(saveEdit(runtime, item, { description: 'Taxi', amount: '8', kind: 'food' }, live)).rejects.toMatchObject(
    {
      code: 'app_field_not_option',
    },
  )
  expect((await store.listItems())[0].action.context).toEqual({ description: 'Taxi', amount: '8', kind: 'food' })
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  expect(await saveEdit(runtime, item, { description: 'Taxi', amount: '8', kind: 'meal' }, live)).toBe(true)
  await waitFor(async () => expect((await store.listItems())[0].state).toBe('synced'))
  expect(actionCalls(calls)[0].body).toMatchObject({
    release: 4,
    context: { description: 'Taxi', amount: '8', kind: 'meal' },
  })
  expect(actionCalls(calls)[0].body).not.toHaveProperty('expectedRevision')
})

// --- review fixes: one version per item, compare-and-set changes, and the server's word for values

// An edit that the server refused as a conflict, with the server's record at `rev`.
async function conflicted(patch: Record<string, unknown> = { amount: 20 }, rev = 2) {
  const ctx = await setup()
  await ctx.transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const server = fakeServer()
  const row = server.rows.get(1) as Row
  Object.assign(row.data, patch)
  row.rev = rev
  await engineOf(ctx.store).run()
  const [item] = await ctx.store.listItems()
  expect(item).toMatchObject({ state: 'attention', attention: 'conflict' })
  return { ...ctx, server, row, item }
}

test('every change to an item raises its version, and a choice made on an older one is refused in the transaction', async () => {
  const { store, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const [first] = await store.listItems()
  expect(first.version).toBe(1)
  await transport.act(press('edit_expense', { id: 1, amount: '10' }, 1))
  const [merged] = await store.listItems()
  expect(merged.version).toBe(2)
  await store.updateItem(merged.id, (i) => ({ ...i, state: 'draft' }))
  const [held] = await store.listItems()
  expect(held.version).toBe(3)
  // A claim's renewal is not a change of what a person was shown.
  await store.updateItem(held.id, (i) => ({ ...i }), { bump: false })
  expect((await store.getItem(held.id))?.version).toBe(3)
  // Discarding what was shown at version 1 is refused, and nothing is removed.
  await expect(store.removeItem(held.id, seenOf(first))).rejects.toBeInstanceOf(ItemChanged)
  expect(await store.listItems()).toHaveLength(1)
  await store.removeItem(held.id, seenOf(held))
  expect(await store.listItems()).toEqual([])
})

test('a stale screen’s edit never joins an item made on another base: refused, and the record is shown again', async () => {
  const { store, d, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const before = await store.listItems()
  // Another tab downloaded the record at a newer revision: this screen still shows revision 1.
  await store.putDownload(download(7, { messages: withRevision(2, { description: 'Coffee', amount: 20 }) }))
  const stale = offlineTransport(runtime, d)
  await stale.load()
  await expect(stale.act(press('edit_expense', { id: 1, description: 'Stale' }, 3))).rejects.toMatchObject({
    code: 'offline_record_changed',
  })
  // The values of one base and the revision of another are never put together.
  expect(await store.listItems()).toEqual(before)

  // The same, when the other tab's item appears between this tab's read of the queue and its write.
  const listed = vi.spyOn(runtime.store, 'listItems').mockResolvedValueOnce([])
  await expect(stale.act(press('edit_expense', { id: 1, description: 'Stale' }, 2))).rejects.toMatchObject({
    code: 'offline_record_changed',
  })
  listed.mockRestore()
  expect(await store.listItems()).toEqual(before)
})

test('a screen drawn from another revision than the device holds does not queue a change that mixes them', async () => {
  const { store, d, runtime } = await setup()
  const stale = offlineTransport(runtime, d)
  await stale.load()
  // Another tab downloaded the record again: the device holds revision 2, the screen shows 1.
  await store.putDownload(download(7, { messages: withRevision(2, { description: 'Coffee', amount: 20 }) }))
  await expect(stale.act(press('edit_expense', { id: 1, description: 'Stale' }, 1))).rejects.toMatchObject({
    code: 'offline_record_changed',
  })
  expect(await store.listItems()).toEqual([])
})

// The download's messages with record 1 at a revision, as another tab's download would have them.
function withRevision(revision: number, fields: Record<string, unknown>): unknown[] {
  return (surface as unknown as { updateDataModel?: { path?: string; value?: unknown } }[]).map((m) =>
    m.updateDataModel?.path === '/'
      ? {
          ...m,
          updateDataModel: {
            ...m.updateDataModel,
            value: { ...(m.updateDataModel.value as object), items: [{ id: 1, _revision: revision, ...fields }] },
          },
        }
      : m,
  )
}

test('a button pressed on a screen resolves in the release the screen was drawn from, never in a newer download', async () => {
  const { store, runtime, d } = await setup()
  const transport = offlineTransport(runtime, d)
  await transport.load()
  // A newer release was downloaded meanwhile, where that name is a deletion.
  await store.putDownload(
    download(7, {
      release: 4,
      definition: { ...definition, actions: { ...definition.actions, edit_expense: { kind: 'delete' } } },
    }),
  )
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const [item] = await store.listItems()
  expect(item).toMatchObject({ kind: 'update', release: 3, action: { name: 'edit_expense' } })
})

test('a different action on the same record is queued on its own, with its own identity; the same action is merged', async () => {
  const two = { ...definition, actions: { ...definition.actions, set_amount: { kind: 'update' as const } } }
  const { store, transport } = await setup({ definition: two })
  await transport.act(press('edit_expense', { id: 1, description: 'Espresso' }, 1))
  await transport.act(press('set_amount', { id: 1, amount: '9' }, 1))
  const items = await store.listItems()
  expect(items.map((i) => i.action.name)).toEqual(['edit_expense', 'set_amount'])
  expect(items.map((i) => i.record.revision)).toEqual([1, 1])
  await transport.act(press('set_amount', { id: 1, amount: '11' }, 1))
  const after = await store.listItems()
  expect(after).toHaveLength(2)
  expect(after[1].action.context).toEqual({ id: 1, amount: '11' })
})

test('a change is not reported as saved when another tab’s deletion of the record won', async () => {
  const { store, runtime, d, transport } = await setup()
  await offlineTransport(runtime, d).act(press('delete_expense', { id: 1 }, 1))
  const before = await store.listItems()
  // This tab had read the queue before the deletion was written.
  const listed = vi.spyOn(runtime.store, 'listItems').mockResolvedValueOnce([])
  await expect(transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))).rejects.toMatchObject({
    code: 'offline_record_changed',
  })
  listed.mockRestore()
  expect(await store.listItems()).toEqual(before)
})

test('applying on top refuses a conflict another tab sent again: it carries the version the person was shown', async () => {
  const { store, runtime, row, item } = await conflicted()
  // Another tab tried again with the same request, and the record moved on once more.
  row.data.amount = 25
  row.rev = 5
  await engineOf(store).retry(item.id)
  const [again] = await store.listItems()
  expect(again).toMatchObject({
    attention: 'conflict',
    operation: item.operation,
    conflict: { current: { _revision: 5 } },
  })
  runtime.engine.gate = true
  await expect(reapply(runtime, item)).rejects.toMatchObject({ code: 'offline_item_changed' })
  expect(await store.listItems()).toEqual([again])
})

test('applying on top asks the server at the moment of the choice: a revision the person was not shown is not applied', async () => {
  const { store, runtime, row, item } = await conflicted()
  // The record moves on after the review was shown, with no change to the item.
  row.data.amount = 30
  row.rev = 3
  runtime.engine.gate = true
  await expect(reapply(runtime, item)).rejects.toMatchObject({ code: 'offline_review_again' })
  await expect(
    saveEdit(runtime, item, { description: 'Coffee', amount: '12' }, (await store.getDownload(scope)) as Download),
  ).rejects.toMatchObject({ code: 'offline_review_again' })
  expect(await store.listItems()).toEqual([item])
  // Shown the record as it is now, the choice is applied on the revision just shown.
  const shown = { id: 1, _revision: 3, description: 'Coffee', amount: 30 }
  expect(await reapply(runtime, item, shown)).toBe(true)
  expect((await store.listItems())[0]).toMatchObject({ record: { id: 1, revision: 3 }, original: { amount: 30 } })
})

test('keeping the server’s version writes the record as the server has it now, and drops one the person may no longer read', async () => {
  const { store, runtime, row, item } = await conflicted()
  // The cached answer holds a record; now the person's access is narrower and the server hides it.
  expect(item.conflict?.current).toMatchObject({ amount: 20 })
  row.hidden = true
  expect(await keepServer(runtime, item)).toBe(true)
  const copy = (await store.getDownload(scope)) as Download
  expect(itemsOf(copy, [])).toEqual([])
  expect(await store.listItems()).toEqual([])
})

test('a conflict is shown with the record as the server has it when it is opened, not as it was answered', async () => {
  const { store, runtime, row, item } = await conflicted()
  row.data.amount = 25
  row.rev = 3
  render(<Harness runtime={runtime} />)
  const entry = await screen.findByText(/Expenses: change to Coffee/)
  fireEvent.click(within(entry.closest('li') as HTMLElement).getByRole('button', { name: /Review/ }))
  const review = await screen.findByRole('region', { name: /Review/ })
  await within(review).findByText('25')
  expect(within(review).queryByText('20')).toBeNull()
  // What is chosen from that screen is applied on that revision.
  runtime.engine.gate = true
  fireEvent.click(within(review).getByRole('button', { name: 'Apply my change on top' }))
  await waitFor(async () => expect((await store.listItems())[0].record.revision).toBe(3))
  expect(item.version).toBeLessThan((await store.listItems())[0].version)
})

test('a replayed answer that carries another device’s revision is shown with that device’s values, never with mine', async () => {
  const { store, d, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  // The server already had this operation (a no-op for it) and answers with the record as it is
  // now: another device changed the amount, which made the next revision.
  fakeFetch({
    [`POST ${base}/actions`]: {
      body: [
        { updateDataModel: { path: '/items', value: [{ id: 1, _revision: 2, description: 'Coffee', amount: 20 }] } },
      ],
    },
  })
  await engineOf(store).run()
  const [synced] = await store.listItems()
  expect(synced.state).toBe('synced')
  expect(itemsOf(d, [synced])).toEqual([{ id: 1, _revision: 2, description: 'Coffee', amount: 20 }])
})

test('signing out loses only what the person was shown: a newer edit of the same record is asked about again', async () => {
  const { store, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const shown = await unsentOf(7)
  // Another tab changes the same item (the same action, so the same item, under a new key).
  await transport.act(press('edit_expense', { id: 1, amount: '11' }, 1))
  const left = await settle(7, approvalOf(shown.items))
  expect(left).toHaveLength(1)
  expect(left[0].version).toBeGreaterThan(shown.items[0].version)
  expect((await store.listItems())[0].action.context).toEqual({ id: 1, amount: '11' })
  // Shown again, as it is now, it is what they agree to lose.
  expect(await settle(7, approvalOf(left))).toEqual([])
})

test('an answer of 401 to a send that may already be on the server does not make the record free: its key is never reused', async () => {
  const { store, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  fakeFetch({ [`POST ${base}/actions`]: { status: 503, body: { error: 'busy' } } })
  const c = clock()
  await engineOf(store, c).run()
  const [sent] = await store.listItems()
  expect(sent).toMatchObject({ state: 'queued', unconfirmed: true })
  // Tried again: the session has ended.
  fakeFetch({ [`POST ${base}/actions`]: { status: 401, body: { error: 'no session', code: 'unauthorized' } } })
  c.advance(10 * 60_000)
  await engineOf(store, c).run()
  const [paused] = await store.listItems()
  expect(paused).toMatchObject({ state: 'queued', unconfirmed: true, operation: sent.operation })
  await expect(transport.act(press('edit_expense', { id: 1, amount: '11' }, 1))).rejects.toMatchObject({
    code: 'offline_record_busy',
  })
  expect((await store.listItems())[0].operation).toBe(sent.operation)
})

test('a send that was never started before the session ended is free again', async () => {
  const { store, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  fakeFetch({ [`POST ${base}/actions`]: { status: 401, body: { error: 'no session', code: 'unauthorized' } } })
  await engineOf(store).run()
  expect((await store.listItems())[0]).toMatchObject({ state: 'queued' })
  expect((await store.listItems())[0].unconfirmed).toBeUndefined()
})

test('a late success after the account changed writes nothing to the old account’s queue', async () => {
  const { store } = await setup()
  const { transport } = { transport: offlineTransport(runtimeOf(store), (await store.getDownload(scope)) as Download) }
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  let signed = 7
  let release: () => void = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  let started: () => void = () => {}
  const inflight = new Promise<void>((resolve) => {
    started = resolve
  })
  fakeFetch({
    [`POST ${base}/actions`]: async () => {
      started()
      await held
      return {
        body: [
          { updateDataModel: { path: '/items', value: [{ id: 1, _revision: 2, description: 'Coffee', amount: 9 }] } },
        ],
      }
    },
  })
  const engine = engineOf(store, clock(), { signedIn: () => signed })
  const running = engine.run()
  await inflight
  // Another account signs in while the request is out; its answer then comes in.
  signed = 8
  release()
  await running
  const [item] = await store.listItems()
  expect(item.state).not.toBe('synced')
  expect(item.ack).toBeUndefined()
  // It is the same request, still unconfirmed, for its owner to send again.
  expect(item.unconfirmed).toBe(true)
})

test('clearing synced records keeps a deleted record out of the screen until a download confirms the deletion', async () => {
  const { store, d, runtime } = await setup()
  await offlineTransport(runtime, d).act(press('delete_expense', { id: 1 }, 1))
  fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  await engineOf(store).run()
  expect((await store.listItems())[0].state).toBe('synced')
  render(<Harness runtime={runtime} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Clear synced records' }))
  await waitFor(async () => expect((await store.listItems())[0].cleared).toBe(true))
  expect(screen.queryByRole('button', { name: 'Clear synced records' })).toBeNull()
  // The copy still holds the record: the deletion is what keeps it from being shown.
  const copy = (await store.getDownload(scope)) as Download
  expect(itemsOf(copy, await store.listItems())).toEqual([])
  // A download that no longer holds the record is what lets the entry go.
  await store.dropIncluded(scope, {}, new Set())
  expect(await store.listItems()).toEqual([])
})

test('a second deletion during a restore offers the restore again, at the deletion it has now', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const server = fakeServer()
  const row = server.rows.get(1) as Row
  row.deleted = true
  row.rev = 2
  await engineOf(store).run()
  const [item] = await store.listItems()
  expect(item).toMatchObject({ attention: 'deleted' })
  await restoreAndApply(runtime, item, { revision: 2, at: null, by: null })
  // Someone restored it and deleted it again before this send.
  row.rev = 4
  await engineOf(store).run()
  const [again] = await store.listItems()
  expect(again).toMatchObject({ state: 'attention', attention: 'deleted' })
  expect(again.restore).toBeUndefined()
  // The restore is offered at the revision of the deletion that is there now.
  const found = await findDeleted(runtime, again)
  expect(found?.revision).toBe(4)
  await expect(restoreAndApply(runtime, again, { revision: 2, at: null, by: null })).rejects.toMatchObject({
    code: 'offline_review_again',
  })
  await restoreAndApply(runtime, again, found as Deleted)
  await engineOf(store).run()
  expect((await store.listItems())[0].state).toBe('synced')
  expect(row).toMatchObject({ deleted: false, rev: 6, data: { amount: 9 } })
})
