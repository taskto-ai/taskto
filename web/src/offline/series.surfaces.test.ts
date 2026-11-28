import { afterEach, expect, test, vi } from 'vitest'
import { answerChange, viewOf } from './local'
import type { Download, OutboxItem } from './store'

afterEach(() => vi.useRealTimers())

const fields = [
  { name: 'kind', type: 'choice', options: ['a', 'b'] },
  { name: 'day', type: 'date' },
]

function download(rows: unknown[], extra: Record<string, unknown> = {}, root: Record<string, unknown> = {}): Download {
  return {
    definition: {
      definition_version: 3,
      requires: ['data.series'],
      collection: { fields },
      actions: { add: { kind: 'insert' } },
      series: {
        by_kind: { kind: 'count', group_by: 'kind' },
        by_month: { kind: 'count', group_by: 'day', bucket: 'month', last: 2 },
      },
      ...extra,
    },
    messages: [
      { version: 'v0.9', createSurface: { surfaceId: 'app-1' } },
      { version: 'v0.9', updateDataModel: { surfaceId: 'app-1', path: '/', value: { items: rows, ...root } } },
    ],
    operations: {},
  } as unknown as Download
}

function queuedInsert(context: Record<string, unknown>): OutboxItem {
  return {
    id: 'q1',
    seq: 1,
    kind: 'insert',
    operation: 'op-1',
    record: { id: -1, revision: null },
    action: { name: 'add', surfaceId: 'app-1', sourceComponentId: 'x', timestamp: '', context },
  } as unknown as OutboxItem
}

const totals = (value: unknown): number[] =>
  (value as { series: { values: (number | null)[] }[] }).series[0].values.map((v) => v ?? 0)

test('the answer to a change offline recomputes every series from the downloaded rows and the queue', () => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-12-15T12:00:00Z'))
  const held = download([{ id: 1, kind: 'a', day: '2026-12-02' }], { screens: [{ name: 'main' }] })
  const out = answerChange(held, [queuedInsert({ kind: 'b', day: '2026-11-09' })], 'add') as {
    updateDataModel: { path: string; value: unknown }
  }[]
  const byPath = Object.fromEntries(out.map((m) => [m.updateDataModel.path, m.updateDataModel.value]))
  expect(totals(byPath['/by_kind'])).toEqual([1, 1])
  expect(totals(byPath['/by_month'])).toEqual([1, 1])
  expect((byPath['/by_month'] as { x: { key: string }[] }).x.map((p) => p.key)).toEqual(['2026-11', '2026-12'])
  expect(out.map((m) => m.updateDataModel.path)).toEqual(['/items', '/by_kind', '/by_month', '/draft'])
})

test('offline numbers come from the rows held on the device, never from the snapshot the server drew', () => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-11-17T12:00:00Z'))
  const stale = { x: [{ key: 'a', label: 'a' }], series: [{ key: '', label: '', values: [99] }] }
  const [, model] = viewOf(download([{ id: 1, kind: 'a' }], {}, { by_kind: stale }), []) as {
    updateDataModel: { value: Record<string, unknown> }
  }[]
  expect(totals(model.updateDataModel.value.by_kind)).toEqual([1, 0])
})

test('the buckets end at the device clock when the view is drawn', () => {
  const keysAt = (now: string): string[] => {
    vi.setSystemTime(new Date(now))
    const [, model] = viewOf(download([]), []) as { updateDataModel: { value: Record<string, unknown> } }[]
    return (model.updateDataModel.value.by_month as { x: { key: string }[] }).x.map((p) => p.key)
  }
  vi.useFakeTimers()
  expect(keysAt('2026-11-17T12:00:00Z')).toEqual(['2026-10', '2026-11'])
  expect(keysAt('2027-01-02T12:00:00Z')).toEqual(['2026-12', '2027-01'])
})
