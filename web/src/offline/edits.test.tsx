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
import {
  type Deleted,
  discard,
  findDeleted,
  keepServer,
  problemsOf,
  reapply,
  restoreAndApply,
  saveEdit,
} from './choices'
import { downloadApp } from './download'
import { useOfflineData } from './hooks'
import { itemsOf } from './local'
import { OfflineApp as OfflineAppView } from './OfflineShell'
import { OutboxList } from './OutboxList'
import { deactivate, rememberUser, runtimeState, setSyncDeps, settle, unsentOf } from './runtime'
import {
  AccountLapsed,
  approvalOf,
  type Download,
  ItemChanged,
  kindOf,
  MIGRATIONS,
  OfflineStore,
  type OutboxItem,
  queueSignature,
  RecordBusy,
  RecordChanged,
  SCHEMA_VERSION,
  scopeKey,
  seenAs,
  seenOf,
} from './store'
import { SyncEngine } from './sync'
import { offlineTransport } from './transport'
import { soleWriter } from './writer.testing'

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
  return { user: store.user, store, engine, stop: () => {}, writer: soleWriter }
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
  await expect(store.discardItem(held.id, seenAs(first))).rejects.toBeInstanceOf(ItemChanged)
  expect(await store.listItems()).toHaveLength(1)
  await store.discardItem(held.id, seenAs(held))
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

// --- second review: one writer, and what each tab's choices and sends may write

// The download's messages with the records given (ids with their revisions and fields).
function withItems(items: unknown[]): unknown[] {
  return (surface as unknown as { updateDataModel?: { path?: string; value?: unknown } }[]).map((m) =>
    m.updateDataModel?.path === '/'
      ? { ...m, updateDataModel: { ...m.updateDataModel, value: { ...(m.updateDataModel.value as object), items } } }
      : m,
  )
}

test('edit and try again applies only the fields the person changed: one they left alone follows the record as it is read again', async () => {
  const { store, runtime, row } = await conflicted({ amount: 20 }, 2)
  render(<Harness runtime={runtime} />)
  const entry = await screen.findByText(/Expenses: change to Coffee/)
  fireEvent.click(within(entry.closest('li') as HTMLElement).getByRole('button', { name: /Review/ }))
  const review = await screen.findByRole('region', { name: /Review/ })
  await within(review).findByText('20')
  fireEvent.click(within(review).getByRole('button', { name: 'Edit and try again' }))
  const form = await screen.findByRole('form', { name: 'Edit and try again' })
  // The person's own change (the amount) and what the server has for the rest.
  expect((within(form).getByLabelText('amount') as HTMLInputElement).value).toBe('9')
  expect((within(form).getByLabelText('description') as HTMLInputElement).value).toBe('Coffee')
  fireEvent.change(within(form).getByLabelText('kind'), { target: { value: 'food' } })
  // The record moves on before they save: the description is somebody else's now.
  row.data.description = 'Tea'
  row.rev = 3
  fireEvent.click(within(form).getByRole('button', { name: 'Save and try again' }))
  await waitFor(() => expect(screen.getByRole('alert').textContent).toMatch(/changed|again/i))
  // The form reads the record again: the field they didn't touch shows the new value, theirs stay.
  await waitFor(() => expect((screen.getByLabelText('description') as HTMLInputElement).value).toBe('Tea'))
  expect((screen.getByLabelText('kind') as HTMLSelectElement).value).toBe('food')
  fireEvent.click(screen.getByRole('button', { name: 'Save and try again' }))
  await waitFor(async () => expect((await store.listItems())[0].state).toBe('queued'))
  const [saved] = await store.listItems()
  expect(saved.record.revision).toBe(3)
  // Only what the person changed: the stale description is not in the request.
  expect(saved.action.context).toEqual({ id: 1, amount: 9, kind: 'food' })
})

// An engine whose requests the test answers by hand.
function held() {
  const waiting: ((answer: unknown) => void)[] = []
  const request = vi.fn(
    () =>
      new Promise((resolve) => {
        waiting.push(resolve)
      }),
  )
  return { request: request as never, waiting, calls: request }
}

test('a late answer is written only for the operation and the attempt it belongs to', async () => {
  const { store, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const c = clock()
  const slow = held()
  const running = engineOf(store, c, { request: slow.request }).run()
  await waitFor(() => expect(slow.calls).toHaveBeenCalled())
  const [sent] = await store.listItems()
  // Meanwhile the person's choice made it another request, under another key, waiting to be sent.
  await store.updateItem(sent.id, (i) => ({
    ...i,
    operation: 'op-replaced-0001',
    state: 'queued',
    lease: undefined,
    unconfirmed: undefined,
  }))
  slow.waiting[0]([])
  await running
  // The first request's answer says nothing of the second: it is not marked synced.
  expect(await store.getItem(sent.id)).toMatchObject({ state: 'queued', operation: 'op-replaced-0001' })
})

test('a late answer of a send that was superseded by another attempt touches neither the state nor the acknowledgement', async () => {
  const a = await OfflineStore.open(7)
  await a.putDownload(download(7))
  const b = await OfflineStore.open(7)
  const { runtime } = { runtime: runtimeOf(a) }
  await offlineTransport(runtime, download(7)).act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const slow = held()
  const early = clock()
  const first = engineOf(a, early, { request: slow.request }).run()
  await waitFor(() => expect(slow.calls).toHaveBeenCalled())
  // Another tab, after the first one's claim ran out, takes the item and has the answer.
  const later = clock()
  later.advance(10 * 60_000)
  const fast = vi.fn(async () => [
    { updateDataModel: { path: '/items', value: [{ id: 1, _revision: 5, description: 'Coffee', amount: 9 }] } },
  ])
  await engineOf(b, later, { request: fast as never }).run()
  expect((await b.listItems())[0]).toMatchObject({ state: 'synced', ack: { _revision: 5 } })
  // The first one's answer comes now, and says another revision.
  slow.waiting[0]([
    { updateDataModel: { path: '/items', value: [{ id: 1, _revision: 2, description: 'Coffee', amount: 9 }] } },
  ])
  await first
  const [after] = await b.listItems()
  expect(after).toMatchObject({ state: 'synced', ack: { _revision: 5 } })
  expect(after.attempts).toBe(1)
})

test('a discard is of the item as the question was asked: one that changed meanwhile is kept, and a synced entry is never discarded', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  render(<Harness runtime={runtime} />)
  const entry = await screen.findByText(/Expenses: change to Coffee/)
  fireEvent.click(within(entry.closest('li') as HTMLElement).getByRole('button', { name: /Discard/ }))
  // The question is open; the item is changed meanwhile (held as a draft in another tab, say).
  const [asked] = await store.listItems()
  await store.updateItem(asked.id, (i) => ({ ...i, state: 'draft' }))
  await screen.findByText('Draft on this device')
  fireEvent.click(screen.getByRole('button', { name: 'Yes, discard' }))
  await screen.findByText(/changed in another tab/)
  expect(await store.listItems()).toHaveLength(1)

  // Whatever the dialog says, an entry the server acknowledged is not removed by a discard.
  fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  await store.updateItem(asked.id, (i) => ({ ...i, state: 'queued' }))
  await engineOf(store).run()
  const [synced] = await store.listItems()
  expect(synced.state).toBe('synced')
  await expect(store.discardItem(synced.id, seenAs(synced))).rejects.toBeInstanceOf(ItemChanged)
  expect(await store.listItems()).toHaveLength(1)
})

test('a conflict choice whose server read was overtaken by a revalidation writes nothing into the copy that replaced it', async () => {
  const { store, runtime, row, item } = await conflicted()
  const server = fakeServer(new Map([[1, row]]))
  void server
  // The revalidation replaces the copy (the person reads fewer records now: the record is gone from
  // it) while the answer to the read of the record is on its way.
  fakeFetch({
    [`GET ${base}/records/1/history`]: async () => {
      await store.putDownload(download(7, { messages: withItems([]), records: 0, total: 0 }))
      return {
        body: {
          record: { id: 1, revision: row.rev, data: row.data, deleted_at: null },
          history: [],
          truncated: false,
        },
      }
    },
  })
  await expect(keepServer(runtime, item)).rejects.toMatchObject({ code: 'offline_copy_changed' })
  // The narrower copy is what is kept: the record the person can't read is not put back.
  const copy = (await store.getDownload(scope)) as Download
  expect(itemsOf(copy, [])).toEqual([])
  expect(await store.getItem(item.id)).toMatchObject({ state: 'attention', attention: 'conflict' })
})

test('a fresh download of another release draws its own screen: a button is resolved in the release its screen came from', async () => {
  const { store, d, runtime } = await setup()
  const { rerender } = render(<OfflineAppView runtime={runtime} download={d} items={[]} />)
  await screen.findByText('Expense tracker')
  const next = download(7, {
    release: 4,
    capturedAt: '2026-11-04T10:00:00.000Z',
    messages: JSON.parse(JSON.stringify(surface).replace('Expense tracker', 'Expense tracker v4')),
  })
  await store.putDownload(next)
  rerender(<OfflineAppView runtime={runtime} download={next} items={[]} />)
  await screen.findByText('Expense tracker v4')
  fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
  await waitFor(async () => expect(await store.listItems()).toHaveLength(1))
  expect((await store.listItems())[0]).toMatchObject({ release: 4, kind: 'delete' })
})

test('an overlay based on one revision is never stamped over a download at another: it waits for review, and a synced no-op never masks a newer download', () => {
  const rev3 = download(7, { messages: withItems([{ id: 1, _revision: 3, description: 'Tea', amount: 20 }]) })
  const queued = {
    kind: 'update',
    state: 'queued',
    seq: 1,
    version: 1,
    operation: 'op-overlay-0001',
    record: { id: 1, revision: 1 },
    action: { name: 'edit_expense', context: { id: 1, amount: '9' } },
    attachments: [],
  } as unknown as OutboxItem
  // The download is at revision 3, the change was made on revision 1: the record is shown as the
  // download has it, for review, never as revision 1's values with a revision they were not made on.
  expect(itemsOf(rev3, [queued])).toEqual([{ id: 1, _revision: 3, _pending: 'review', description: 'Tea', amount: 20 }])
  // On the revision the download holds, it is the person's change, said to be waiting.
  const rev1 = download(7, { messages: withItems([{ id: 1, _revision: 1, description: 'Coffee', amount: 4.5 }]) })
  expect(itemsOf(rev1, [queued])).toEqual([
    { id: 1, _revision: 1, _pending: 'update', description: 'Coffee', amount: 9 },
  ])
  // A change the server has, that said nothing of the record it has: it does not cover a newer copy.
  const noop = { ...queued, state: 'synced' } as OutboxItem
  expect(itemsOf(rev3, [noop])).toEqual([{ id: 1, _revision: 3, description: 'Tea', amount: 20 }])
  expect(itemsOf(rev1, [noop])).toEqual([{ id: 1, description: 'Coffee', amount: 9 }])
  // One that did: the server's record, unless the copy is newer than its answer.
  const acked = (revision: number) =>
    ({ ...queued, state: 'synced', ack: { id: 1, _revision: revision, description: 'Mine', amount: 9 } }) as OutboxItem
  expect(itemsOf(rev3, [acked(2)])).toEqual([{ id: 1, _revision: 3, description: 'Tea', amount: 20 }])
  expect(itemsOf(rev3, [acked(4)])).toEqual([{ id: 1, _revision: 4, description: 'Mine', amount: 9 }])
})

test('an enqueue looks at every item of the record in its transaction: any that may be on the server blocks it, and any deletion refuses it', async () => {
  const { store } = await setup()
  const draft = (over: Partial<OutboxItem>) => ({
    kind: 'update' as const,
    id: `item-${Math.random()}`,
    user: 7,
    scopeKey: scopeKey(scope),
    ...scope,
    base,
    release: 3,
    schema: 1,
    action: { name: 'edit_expense', surfaceId: 'app-1', sourceComponentId: '', timestamp: '', context: { id: 1 } },
    operation: `op-${Math.random().toString(36).slice(2, 12)}`,
    record: { id: 1, revision: 1 },
    attachments: [],
    state: 'queued' as const,
    attempts: 0,
    createdAt: '',
    updatedAt: '',
    ...over,
  })
  const fresh = draft({
    action: { name: 'other', surfaceId: 'app-1', sourceComponentId: '', timestamp: '', context: { id: 1 } },
  })
  // An earlier item gave up after it was sent and may be on the server; a later one waits behind it.
  const early = await store.enqueue(draft({ state: 'attention', attention: 'gave_up', unconfirmed: true }), [])
  await store.enqueue(
    draft({ action: { name: 'other', surfaceId: 'app-1', sourceComponentId: '', timestamp: '', context: { id: 1 } } }),
    [],
  )
  await expect(store.changeRecord(scope, 1, () => fresh)).rejects.toBeInstanceOf(RecordBusy)
  await store.removeItem(early.id)
  // A deletion, even one the server has answered, is the end of the record's edits.
  const gone = await store.enqueue(draft({ kind: 'delete', state: 'synced' }), [])
  await expect(store.changeRecord(scope, 1, () => fresh)).rejects.toBeInstanceOf(RecordChanged)
  await store.removeItem(gone.id)
  // The queue a screen was drawn from is the queue the transaction sees: an item that was
  // acknowledged since is a record at another revision.
  const [mine] = await store.listItems(scope)
  const drawn = queueSignature(await store.listItems(scope))
  await store.updateItem(mine.id, (i) => ({ ...i, state: 'synced' }), { bump: false })
  await expect(store.changeRecord(scope, 1, () => fresh, drawn)).rejects.toBeInstanceOf(RecordChanged)
})

test('an edit pressed while an earlier item of the record synced after the screen was read is refused, not based on a stale revision', async () => {
  const { store, runtime, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  const [first] = await store.listItems()
  // The queue is read for the screen, and the item is acknowledged before the change is written.
  const list = runtime.store.listItems.bind(runtime.store)
  vi.spyOn(runtime.store, 'listItems').mockImplementationOnce(async (s) => {
    const items = await list(s)
    await store.updateItem(first.id, (i) => ({ ...i, state: 'synced', syncedAt: 'now', unconfirmed: undefined }), {
      bump: false,
    })
    return items
  })
  await expect(
    transport.act(press('edit_expense', { id: 1, description: 'Later', amount: '1' }, 1)),
  ).rejects.toMatchObject({ code: 'offline_record_changed' })
  expect(await store.listItems()).toHaveLength(1)
})

test('an answer that is a refusal made before the operation was looked at leaves the change unconfirmed: only the server’s verdict on it clears that', async () => {
  const { store, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const c = clock()
  fakeFetch({ [`POST ${base}/actions`]: { status: 503, body: { error: 'busy' } } })
  await engineOf(store, c).run()
  expect((await store.listItems())[0]).toMatchObject({ state: 'queued', unconfirmed: true })
  // Retried after access was lost: a refusal that says nothing of the first send.
  fakeFetch({
    [`POST ${base}/actions`]: { status: 403, body: { error: 'no', code: 'app_forbidden' } },
    [`GET ${base}`]: { body: {} },
  })
  c.advance(10 * 60_000)
  await engineOf(store, c).run()
  const [refused] = await store.listItems()
  expect(refused).toMatchObject({ state: 'attention', attention: 'access', unconfirmed: true })
  await expect(transport.act(press('edit_expense', { id: 1, amount: '11' }, 1))).rejects.toMatchObject({
    code: 'offline_record_busy',
  })
  // A verdict on the operation itself (the record is gone) is what says the server doesn't have it.
  fakeFetch({ [`POST ${base}/actions`]: { status: 422, body: { error: 'no row', code: 'app_row_not_found' } } })
  await engineOf(store, c).retry(refused.id)
  expect((await store.listItems())[0]).toMatchObject({ state: 'attention', attention: 'deleted' })
  expect((await store.listItems())[0].unconfirmed).toBeUndefined()
})

test('the account is checked in the transaction that writes the acknowledgement', async () => {
  const { store, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  let who = 7
  let answered = false
  fakeFetch({
    [`POST ${base}/actions`]: () => {
      answered = true
      return { body: [] }
    },
  })
  // Another account signs in just as the answer is about to be written: after the send's own checks.
  const write = store.updateItem.bind(store)
  vi.spyOn(store, 'updateItem').mockImplementation((id, change, options) => {
    if (answered) who = 8
    return write(id, change, options)
  })
  await engineOf(store, clock(), { signedIn: () => who }).run()
  expect((await store.getItem((await store.listItems())[0].id))?.state).not.toBe('synced')
})

test('a change to a record waits until the earlier change to it has an answer for good, so a delete never overtakes the edit before it', async () => {
  const { store, runtime } = await setup()
  const t = offlineTransport(runtime, download(7))
  await t.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  await t.act(press('delete_expense', { id: 1 }, 1))
  expect((await store.listItems()).map((i) => i.kind)).toEqual(['update', 'delete'])
  const server = fakeServer()
  // The edit's send is not answered: it waits, and may be on the server.
  const real = server.calls
  void real
  const failing = fakeFetch({ [`POST ${base}/actions`]: { status: 503, body: { error: 'busy' } } })
  const c = clock()
  const engine = engineOf(store, c)
  await engine.run()
  expect(actionCalls(failing).map((x) => (x.body as { name: string }).name)).toEqual(['edit_expense'])
  // Again, before the edit's wait is over: the delete is due, but not before the edit has its answer.
  await engine.run()
  expect(actionCalls(failing).map((x) => (x.body as { name: string }).name)).toEqual(['edit_expense'])
  // The edit is answered, then the delete goes (and finds what the edit made of the record).
  const ok = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  c.advance(10 * 60_000)
  await engineOf(store, c).run()
  expect(actionCalls(ok).map((x) => (x.body as { name: string }).name)).toEqual(['edit_expense', 'delete_expense'])
})

test('in a tab that is not the writer a button that changes a record says so, and writes nothing', async () => {
  const { store, d, runtime } = await setup()
  const reader = { ...runtime, writer: { ...soleWriter, holds: () => false } } as unknown as typeof runtime
  const transport = offlineTransport(reader, d)
  await expect(transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))).rejects.toMatchObject({
    code: 'offline_not_writer',
  })
  await expect(transport.act(press('delete_expense', { id: 1 }, 1))).rejects.toMatchObject({
    code: 'offline_not_writer',
  })
  expect(await store.listItems()).toEqual([])
  // The screen says it in words.
  render(<OfflineAppView runtime={reader} download={d} items={[]} />)
  await screen.findByText('Coffee')
  fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/Changes are being made in another tab/)
  expect(await store.listItems()).toEqual([])
})

test('a send whose tab was lost after it began stays unconfirmed when the next tab is refused at the route: only a verdict on the operation clears it', async () => {
  const { store, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  // The first tab began the send and was never heard of again: its request is never answered.
  const lost = held()
  const first = engineOf(store, clock(), { request: lost.request }).run()
  await waitFor(() => expect(lost.calls).toHaveBeenCalled())
  expect((await store.listItems())[0]).toMatchObject({ state: 'syncing', unconfirmed: true })
  // Another tab takes the item once the claim ran out, and the route refuses it: the space or the
  // app says no, before it looked at the operation, which says nothing of what the first send did.
  const later = clock()
  later.advance(10 * 60_000)
  fakeFetch({
    [`POST ${base}/actions`]: { status: 404, body: { error: 'no', code: 'space_not_found' } },
    [`GET ${base}`]: { body: {} },
  })
  await engineOf(store, later).run()
  const [refused] = await store.listItems()
  expect(refused).toMatchObject({ state: 'attention', attention: 'access', unconfirmed: true })
  // It still takes no further change: the first send may have committed.
  await expect(transport.act(press('edit_expense', { id: 1, amount: '11' }, 1))).rejects.toMatchObject({
    code: 'offline_record_busy',
  })
  lost.waiting[0]([])
  await first
  // Its late answer wrote nothing.
  expect((await store.listItems())[0]).toMatchObject({ state: 'attention', unconfirmed: true })
})

test('a claim is refused in its own transaction while an earlier change to the same record has no answer for good, so a delete never overtakes it', async () => {
  const { store, runtime } = await setup()
  const t = offlineTransport(runtime, download(7))
  await t.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  await t.act(press('delete_expense', { id: 1 }, 1))
  const [edit] = await store.listItems()
  // The edit needs attention, and was not left unconfirmed: the delete may go.
  await store.updateItem(edit.id, (i) => ({ ...i, state: 'attention', attention: 'conflict' }))
  // The pass reads the queue and picks the delete; before it claims, the person (in the writer) puts
  // the edit back to waiting.
  const list = store.listItems.bind(store)
  vi.spyOn(store, 'listItems').mockImplementationOnce(async (s) => {
    const items = await list(s)
    await store.updateItem(edit.id, (i) => ({ ...i, state: 'queued', attention: undefined }))
    return items
  })
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  await engineOf(store).run()
  // The delete was not claimed ahead of the edit: the edit is sent first, then the delete.
  expect(actionCalls(calls).map((x) => (x.body as { name: string }).name)).toEqual(['edit_expense', 'delete_expense'])
  expect((await store.listItems()).map((i) => i.state)).toEqual(['synced', 'synced'])
})

// The download's messages with no record in them: what a server that deleted it answers.
const withoutRecords = () =>
  surface.map((m) =>
    m.updateDataModel
      ? { ...m, updateDataModel: { ...m.updateDataModel, value: { draft: {}, items: [], total: '0.00' } } }
      : m,
  )

test('a download that began earlier never replaces a newer one that was written first: a deleted record does not come back', async () => {
  const { store, runtime, transport } = await setup()
  // The record was deleted here and the server has it: a synced deletion, held until a download says so.
  await transport.act(press('delete_expense', { id: 1 }, 1))
  const [gone] = await store.listItems()
  await store.updateItem(gone.id, (i) => ({ ...i, state: 'synced' }))
  let release: () => void = () => {}
  const opened = new Promise<void>((resolve) => {
    release = resolve
  })
  let asked = 0
  const calls = fakeFetch({
    [`GET ${base}/offline`]: async () => {
      // The first request is answered late, from before the deletion: it still holds the record.
      if (asked++ === 0) {
        await opened
        return { body: offlineApp({ captured_at: '2026-11-04T10:00:00.000Z' }) }
      }
      return {
        body: {
          ...offlineApp({ captured_at: '2026-11-04T10:00:05.000Z' }),
          messages: withoutRecords(),
          records: 0,
          total: 0,
          modifiable: [],
        },
      }
    },
  })
  const older = downloadApp(store, scope)
  await waitFor(() => expect(calls.length).toBe(1))
  // A newer download begins, is answered first and written: the record is gone, and its deletion's
  // note goes with it.
  const newer = await downloadApp(store, scope)
  expect(itemsOf(newer, [])).toEqual([])
  expect(await store.listItems()).toEqual([])
  // The older answer arrives now. It is not written over the newer copy.
  release()
  const answer = await older
  expect(answer.capturedAt).toBe('2026-11-04T10:00:05.000Z')
  const copy = (await store.getDownload(scope)) as Download
  expect(copy.capturedAt).toBe('2026-11-04T10:00:05.000Z')
  expect(itemsOf(copy, await store.listItems())).toEqual([])
  expect(runtime.user).toBe(7)
})

test('a download that began before the copy was removed does not bring it back', async () => {
  const { store } = await setup()
  let release: () => void = () => {}
  const opened = new Promise<void>((resolve) => {
    release = resolve
  })
  const calls = fakeFetch({
    [`GET ${base}/offline`]: async () => {
      await opened
      return { body: offlineApp() }
    },
  })
  const pending = downloadApp(store, scope)
  await waitFor(() => expect(calls.length).toBe(1))
  // Access was revoked meanwhile: the copy is removed.
  await store.removeDownload(scope)
  release()
  await expect(pending).rejects.toMatchObject({ name: 'StaleDownload' })
  expect(await store.getDownload(scope)).toBeUndefined()
  // A download that begins after the removal is kept.
  fakeFetch({ [`GET ${base}/offline`]: { body: offlineApp() } })
  expect((await downloadApp(store, scope)).release).toBe(3)
})

// --- the fourth review

// An item written straight into the queue: a change to record 1 that waits, as the tests set it.
const waiting = (over: Partial<OutboxItem> = {}) => ({
  kind: 'update' as const,
  id: `item-${Math.random()}`,
  user: 7,
  scopeKey: scopeKey(scope),
  ...scope,
  base,
  release: 3,
  schema: 1,
  action: { name: 'edit_expense', surfaceId: 'app-1', sourceComponentId: '', timestamp: '', context: { id: 1 } },
  operation: `op-${Math.random().toString(36).slice(2, 12)}`,
  record: { id: 1, revision: 1 },
  attachments: [],
  state: 'queued' as const,
  attempts: 0,
  createdAt: '',
  updatedAt: '',
  ...over,
})

test('a reply to a send that lost its claim is not acted on: a stale 404 removes no fresh download', async () => {
  const { store, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const [item] = await store.listItems()
  fakeFetch({
    [`POST ${base}/actions`]: { status: 404, body: { error: 'nf', code: 'not_found' } },
    // While the probe is on its way another send has the item (its claim and token are not ours).
    [`GET ${base}`]: async () => {
      await store.updateItem(
        item.id,
        (i) => ({ ...i, lease: { owner: 'another-tab', until: Number.MAX_SAFE_INTEGER, attempt: 'theirs' } }),
        { bump: false },
      )
      return { status: 404, body: { error: 'nf', code: 'not_found' } }
    },
  })
  const revoked = vi.fn(async () => store.removeDownload(scope))
  await engineOf(store, clock(), { onRevoked: revoked }).run()
  expect(revoked).not.toHaveBeenCalled()
  expect(await store.listDownloads()).toHaveLength(1)
  // Nothing of the reply was written either: the item is the other send's.
  expect(await store.getItem(item.id)).toMatchObject({ state: 'syncing', lease: { owner: 'another-tab' } })
})

test('a download is written only while the account is the database’s, asked in the writing transaction after what it read', async () => {
  const store = await OfflineStore.open(7)
  // The account is right when the write begins and has changed by the time its reads are done.
  const ticket = await store.beginDownload()
  let asked = 0
  store.requireAccount(() => ++asked === 1)
  await expect(store.putDownload(download(7), ticket)).rejects.toBeInstanceOf(AccountLapsed)
  store.requireAccount(() => true)
  expect(await store.getDownload(scope)).toBeUndefined()
})

test('a discard confirmed after the item went out and came back to the queue is refused: it may be on the server', async () => {
  const { store, transport } = await setup()
  await transport.act(press('edit_expense', { id: 1, amount: '9' }, 1))
  const [queued] = await store.listItems()
  // The question is opened on the queued item.
  const asked = seenAs(queued)
  // A send is made and not answered for good: queued → syncing → queued, the same version and key.
  const c = clock()
  fakeFetch({ [`POST ${base}/actions`]: { status: 503, body: { error: 'busy' } } })
  await engineOf(store, c).run()
  const [again] = await store.listItems()
  expect(again).toMatchObject({ state: 'queued', version: queued.version, operation: queued.operation })
  await expect(store.discardItem(again.id, asked)).rejects.toBeInstanceOf(ItemChanged)
  expect(await store.listItems()).toHaveLength(1)
  // Asked again about the item as it is now, it is the person's to discard.
  await store.discardItem(again.id, seenAs(again))
  expect(await store.listItems()).toEqual([])
})

test('a successor whose own wait is over is not woken by it while an earlier change to its record waits: no zero-delay timer', async () => {
  const { store } = await setup()
  const c = clock()
  await store.enqueue(waiting({ nextAttemptAt: c.now() + 60_000 }), [])
  await store.enqueue(waiting({ nextAttemptAt: c.now() - 5_000 }), [])
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  const timers: { fn: () => void; ms: number }[] = []
  const engine = new SyncEngine(store, {
    now: c.now,
    setTimer: (fn, ms) => timers.push({ fn, ms }),
    clearTimer: () => {},
    random: () => 0.5,
  })
  await engine.run()
  expect(actionCalls(calls)).toHaveLength(0)
  // Woken when the earlier one is due, and not before.
  expect(timers.map((t) => t.ms)).toEqual([60_000])
})

test('discarding the earlier change wakes the queue: the change that waited for it is sent at once', async () => {
  const { store, runtime } = await setup()
  const first = await store.enqueue(waiting({ nextAttemptAt: clock().now() + 600_000 }), [])
  await store.enqueue(waiting(), [])
  const calls = fakeFetch({ [`POST ${base}/actions`]: { body: [] } })
  runtime.engine.gate = true
  await discard(runtime, first, seenAs(first))
  expect(actionCalls(calls)).toHaveLength(1)
  expect((await store.listItems()).map((i) => i.state)).toEqual(['synced'])
})
