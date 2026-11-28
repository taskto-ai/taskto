import { afterEach, expect, test, vi } from 'vitest'
import fixture from '../../../internal/apps/testdata/series.json'
import { viewOf } from './local'
import type { Download } from './store'

afterEach(() => vi.useRealTimers())

test('offline series match the shared server fixture', () => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date(fixture.now))
  const download = {
    definition: {
      definition_version: 3,
      requires: ['data.series'],
      collection: fixture.collection,
      actions: {},
      series: fixture.series,
    },
    messages: [
      { version: 'v0.9', createSurface: { surfaceId: 'app-1' } },
      { version: 'v0.9', updateDataModel: { surfaceId: 'app-1', path: '/', value: { items: fixture.rows } } },
    ],
    operations: {},
  } as unknown as Download
  const [, model] = viewOf(download, []) as { updateDataModel: { value: Record<string, unknown> } }[]
  for (const [name, expected] of Object.entries(fixture.expected)) {
    expect(model.updateDataModel.value[name], name).toEqual(expected)
  }
  expect(Object.keys(fixture.expected)).toEqual(Object.keys(fixture.series).sort())
})

function modelOf(fields: unknown[], rows: unknown[], series: unknown): Record<string, unknown> {
  const download = {
    definition: { definition_version: 3, requires: ['data.series'], collection: { fields }, actions: {}, series },
    messages: [
      { version: 'v0.9', createSurface: { surfaceId: 'app-1' } },
      { version: 'v0.9', updateDataModel: { surfaceId: 'app-1', path: '/', value: { items: rows } } },
    ],
    operations: {},
  } as unknown as Download
  const [, model] = viewOf(download, []) as { updateDataModel: { value: Record<string, unknown> } }[]
  return model.updateDataModel.value
}

test('a label is read only from the field’s own option_labels', () => {
  const value = modelOf(
    [{ name: 'kind', type: 'choice', options: ['constructor', 'worker'], option_labels: { worker: 'Worker' } }],
    [],
    { by_kind: { kind: 'count', group_by: 'kind' } },
  )
  expect((value.by_kind as { x: unknown[] }).x).toEqual([
    { key: 'constructor', label: 'constructor' },
    { key: 'worker', label: 'Worker' },
  ])
})

test('text ties break by UTF-8 bytes like the server', () => {
  const names = ['Ａ', '\u{1F600}', ...Array.from({ length: 19 }, (_, i) => `k${100 + i}`)]
  const value = modelOf(
    [{ name: 'name', type: 'string' }],
    names.map((name, id) => ({ id, name })),
    { by_name: { kind: 'count', group_by: 'name' } },
  )
  expect((value.by_name as { x: { key: string }[] }).x[19].key).toBe('Ａ')
})

test('an overflowing sum is null and an average of huge values is kept', () => {
  const rows = [
    { id: 1, amount: Number.MAX_VALUE, kind: 'a' },
    { id: 2, amount: Number.MAX_VALUE, kind: 'a' },
  ]
  const value = modelOf(
    [
      { name: 'amount', type: 'number' },
      { name: 'kind', type: 'choice', options: ['a'] },
    ],
    rows,
    {
      total: { kind: 'sum', field: 'amount', group_by: 'kind' },
      mean: { kind: 'avg', field: 'amount', group_by: 'kind' },
    },
  )
  expect((value.total as { series: { values: unknown[] }[] }).series[0].values).toEqual([null])
  expect(
    (
      modelOf(
        [
          { name: 'amount', type: 'number' },
          { name: 'kind', type: 'choice', options: ['a'] },
        ],
        [
          { id: 1, amount: Number.MAX_VALUE, kind: 'a' },
          { id: 2, amount: -Number.MAX_VALUE, kind: 'a' },
        ],
        { mean: { kind: 'avg', field: 'amount', group_by: 'kind' } },
      ).mean as { series: { values: unknown[] }[] }
    ).series[0].values,
  ).toEqual([0])
  expect((value.mean as { series: { values: unknown[] }[] }).series[0].values).toEqual([Number.MAX_VALUE])
})
