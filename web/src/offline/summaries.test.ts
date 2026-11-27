import { afterEach, expect, test, vi } from 'vitest'
import { viewOf } from './local'
import type { Download, OutboxItem } from './store'

afterEach(() => vi.useRealTimers())

function download(items: Record<string, unknown>[]): Download {
  return {
    definition: {
      definition_version: 2,
      requires: [],
      collection: { fields: [] },
      actions: {},
      summaries: {
        month: { kind: 'sum', field: 'amount', month_of: 'spent_on' },
        all: { kind: 'sum', field: 'amount' },
        open: { kind: 'count', where: { field: 'status', op: 'eq', value: 'out' } },
      },
    },
    messages: [
      { version: 'v0.9', createSurface: { surfaceId: 'app-1' } },
      { version: 'v0.9', updateDataModel: { surfaceId: 'app-1', path: '/', value: { items } } },
    ],
    operations: {},
  } as unknown as Download
}

test('a monthly summary offline leaves out a row whose date does not exist, as the server does', () => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-04-15T12:00:00Z'))
  const items = [
    { id: 1, amount: 10, spent_on: '2026-04-30' },
    { id: 2, amount: 5, spent_on: '2026-04-31' },
    { id: 3, amount: 2, spent_on: '2026-04-00' },
  ]
  const [, data] = viewOf(download(items), []) as { updateDataModel: { value: Record<string, string> } }[]
  expect(data.updateDataModel.value.month).toBe('10.00')
})

test('a monthly summary offline counts the rows dated in the current month, as the server does', () => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-11-23T12:00:00Z'))
  const items = [
    { id: 1, amount: 10, spent_on: '2026-11-02', status: 'out' },
    { id: 2, amount: 5, spent_on: '2026-10-31', status: 'back' },
    { id: 3, amount: 2.5, spent_on: '2026-11-30' },
    { id: 4, amount: 100, spent_on: '2025-11-15' },
  ]
  const [, data] = viewOf(download(items), []) as { updateDataModel: { value: Record<string, string> } }[]
  expect(data.updateDataModel.value.month).toBe('12.50')
  expect(data.updateDataModel.value.all).toBe('117.50')
  expect(data.updateDataModel.value.open).toBe('1')
})

test('a queued insert the server will refuse is neither drawn nor counted offline', () => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-02-15T12:00:00Z'))
  const d = download([{ id: 1, amount: 10, spent_on: '2026-02-02' }])
  d.definition.collection.fields = [
    { name: 'amount', type: 'number' },
    { name: 'spent_on', type: 'date' },
  ] as Download['definition']['collection']['fields']
  d.definition.summaries = { count: { kind: 'count' }, all: { kind: 'sum', field: 'amount' } }
  d.definition.actions = { add: {} } as unknown as Download['definition']['actions']
  const queued = (seq: number, spent_on: string) =>
    ({
      id: `q${seq}`,
      seq,
      operation: `op${seq}`,
      kind: 'insert',
      record: { id: -seq, revision: null },
      action: { name: 'add', context: { amount: 15, spent_on } },
    }) as unknown as OutboxItem
  const [, data] = viewOf(d, [queued(1, '2026-02-30'), queued(2, '2026-02-10')]) as {
    updateDataModel: { value: { items: { id: number }[]; count: string; all: string } }
  }[]
  expect(data.updateDataModel.value.items.map((i) => i.id)).toEqual([1, -2])
  expect(data.updateDataModel.value.count).toBe('2')
  expect(data.updateDataModel.value.all).toBe('25.00')
})
