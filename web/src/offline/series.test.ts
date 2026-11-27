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
