// @vitest-environment jsdom

import 'fake-indexeddb/auto'
import { Blob as NodeBlob } from 'node:buffer'
import { IDBFactory } from 'fake-indexeddb'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { OfflineApp } from '../api'
import { forgetOperations } from '../apps/operations'
import { fakeFetch } from '../test/fakes'
import { itemsOf, parseInsert } from './local'
import { deactivate, rememberUser, setSyncDeps } from './runtime'
import { type Download, OfflineStore, scopeKey } from './store'
import { SyncEngine } from './sync'
import { offlineTransport } from './transport'
import { soleWriter } from './writer.testing'

const scope = { workspace: 1, space: 2, app: 1 }
const base = '/api/workspaces/1/spaces/2/apps/1'
const blob = (data: string, type = '') => new NodeBlob([data], { type }) as unknown as Blob

const definition = {
  definition_version: 2,
  requires: ['field.barcode', 'field.location', 'field.signature'],
  collection: {
    fields: [
      { name: 'title', type: 'string' },
      { name: 'code', type: 'barcode', format: 'ean_13', max_length: 20, required: true },
      { name: 'place', type: 'location', capture: 'button', required: true },
      { name: 'signed', type: 'signature', max_bytes: 1048576, required: true },
    ],
  },
  actions: { add: { kind: 'insert' } },
} as unknown as OfflineApp['definition']

function download(user: number): Download {
  return {
    ...scope,
    key: scopeKey(scope),
    user,
    name: 'Inspection',
    release: 3,
    definition,
    messages: [],
    scopeOf: 'all',
    records: 0,
    total: 0,
    truncated: false,
    canInsert: true,
    capturedAt: '2026-11-02T10:00:00.000Z',
    bytes: 100,
    operations: {},
  }
}

const press = (context: Record<string, unknown>) => ({
  name: 'add',
  surfaceId: 'app-1',
  sourceComponentId: 'go',
  timestamp: '2026-11-02T10:00:00.000Z',
  context,
})

const place = (over: Record<string, unknown> = {}) => ({
  lat: 38.7223,
  lon: -9.1393,
  accuracy_m: 12.5,
  captured_at: new Date(Date.now() - 60_000).toISOString(),
  source: 'device',
  ...over,
})
const sig = 'ab'.repeat(16)

beforeEach(() => {
  globalThis.indexedDB = new IDBFactory()
  window.localStorage.clear()
  rememberUser({ id: 7, name: 'Pat' })
  forgetOperations()
})

afterEach(async () => {
  vi.restoreAllMocks()
  setSyncDeps({})
  await deactivate({ purge: false })
})

const refusal = (context: Record<string, unknown>) => {
  try {
    parseInsert(definition, context)
  } catch (e) {
    return (e as { code?: string }).code
  }
  return undefined
}
const good = () => ({ code: '4006381333931', place: place(), signed: sig })

test('a record with a required code, location and signature parses offline, as the server stores it', () => {
  const values = parseInsert(definition, { ...good(), title: 'Gate', signed: [sig] })
  expect(values).toMatchObject({ code: '4006381333931', signed: sig, place: { lat: 38.7223, source: 'device' } })
  expect((values.place as { captured_at: string }).captured_at).toMatch(/^\d{4}-\d\d-\d\dT[\d:.]+Z$/)
  expect(refusal({ ...good(), code: '' })).toBe('app_field_required')
  expect(refusal({ ...good(), place: {} })).toBe('app_field_required')
  expect(refusal({ ...good(), signed: [] })).toBe('app_field_required')
})

test('a barcode is checked offline as the server checks it', () => {
  for (const code of ['4006381333930', '400638133393', 'ABCDEFGHIJKLM', '4006381333931\n', '\t4006381333931']) {
    expect(refusal({ ...good(), code }), JSON.stringify(code)).toBe('app_field_not_barcode')
  }
  expect(refusal({ ...good(), code: ' 4006381333931 ' })).toBeUndefined()
  expect(refusal({ ...good(), code: 12 })).toBe('app_field_not_barcode')
  const upce = { ...definition, collection: { fields: [{ name: 'c', type: 'barcode', max_length: 4 }] } }
  expect(() => parseInsert(upce as never, { c: 'ABCDE' })).toThrow(
    expect.objectContaining({ code: 'app_field_too_long' }),
  )
})

test('a location is checked offline as the server checks it', () => {
  const bad: Record<string, unknown> = {
    'lat over': place({ lat: 90.1 }),
    'lon under': place({ lon: -180.5 }),
    'lat as text': place({ lat: '38.7' }),
    'device without accuracy': place({ accuracy_m: undefined }),
    'negative accuracy': place({ accuracy_m: -1 }),
    'other source': place({ source: 'gps' }),
    'no time': place({ captured_at: undefined }),
    'date only': place({ captured_at: '2026-11-20' }),
    'before 2000': place({ captured_at: '1999-12-31T23:59:59Z' }),
    'a day that is not one': place({ captured_at: '2026-02-30T10:00:00Z' }),
    'offset +24:60': place({ captured_at: '2026-11-20T10:00:00+24:60' }),
    'offset +24:00': place({ captured_at: '2026-11-20T10:00:00+24:00' }),
    'unknown key': place({ altitude: 3 }),
    text: '38.7,-9.1',
    list: [38.7, -9.1],
  }
  for (const [name, value] of Object.entries(bad)) {
    expect(refusal({ ...good(), place: value }), name).toBe('app_field_not_location')
  }
  expect(refusal({ ...good(), place: place({ captured_at: new Date(Date.now() + 3_600_000).toISOString() }) })).toBe(
    'app_field_location_future',
  )
  expect(refusal({ ...good(), place: place({ source: 'manual', accuracy_m: undefined }) })).toBeUndefined()
  expect(refusal({ ...good(), place: place({ lat: -90, lon: 180, accuracy_m: 0 }) })).toBeUndefined()
  expect(refusal({ ...good(), place: place({ captured_at: '2026-11-20T10:00:00.250+05:30' }) })).toBeUndefined()
})

test('a signature is one attachment id offline too', () => {
  for (const signed of ['me', 'AB'.repeat(16), [sig, sig], [1], 12, { id: sig }]) {
    expect(refusal({ ...good(), signed }), JSON.stringify(signed)).toBe('app_field_not_signature')
  }
})

test('offline, a record with a required code, location and signature is queued, with the signature’s picture', async () => {
  const store = await OfflineStore.open(7)
  const d = download(7)
  await store.putDownload(d)
  const runtime = {
    user: 7,
    store,
    engine: { run: async () => {} } as unknown as SyncEngine,
    stop: () => {},
    writer: soleWriter,
  }
  const transport = offlineTransport(runtime, d)
  const picture = blob('png bytes', 'image/png')
  const staged = transport.photos.stage(picture)
  await transport.act(press({ ...good(), signed: staged, title: 'Gate' }))
  const [item] = await store.listItems()
  expect(item.attachments).toEqual([
    expect.objectContaining({ localId: staged, field: 'signed', state: 'pending', type: 'image/png' }),
  ])
  expect(await (await store.getBlob(item.attachments[0].blobId))?.text()).toBe('png bytes')
  const [shown] = itemsOf(d, [item]).filter((i) => i.title === 'Gate')
  expect(shown).toMatchObject({ code: '4006381333931', signed: staged })
  await expect(transport.act(press({ ...good(), signed: 'e'.repeat(32) }))).rejects.toMatchObject({
    code: 'app_attachment_bad',
  })
  expect(await store.listItems()).toHaveLength(1)

  const sent: unknown[] = []
  fakeFetch({
    [`POST ${base}/actions`]: (call) => {
      sent.push((call.body as { context: Record<string, unknown> }).context)
      return { body: [] }
    },
  })
  const upload = vi.fn(async (_base: string, field: string, file: Blob) => ({
    id: 'f'.repeat(32),
    field,
    status: 'ready' as const,
    size: file.size,
    received: file.size,
    attached: false,
  }))
  const engine = new SyncEngine(store, {
    now: () => 1_000_000,
    setTimer: () => 0,
    clearTimer: () => {},
    random: () => 0.5,
    upload,
  })
  await engine.run()
  expect(upload).toHaveBeenCalledWith(base, 'signed', expect.anything(), expect.anything())
  expect(sent).toEqual([expect.objectContaining({ signed: 'f'.repeat(32), code: '4006381333931' })])
  expect((await store.listItems())[0].state).toBe('synced')
})
