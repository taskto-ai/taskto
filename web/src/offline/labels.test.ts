import { expect, test } from 'vitest'
import { itemsOf } from './local'
import type { Download, OutboxItem } from './store'

const definition = {
  definition_version: 2,
  requires: [],
  collection: {
    fields: [
      { name: 'description', type: 'string' },
      {
        name: 'kind',
        type: 'choice',
        options: ['food', 'travel', 'other'],
        option_labels: { food: 'Food', travel: 'Travel' },
      },
    ],
  },
  actions: { add: { kind: 'insert' }, edit: { kind: 'update' } },
}

const copy = (items: unknown[]) =>
  ({
    definition,
    operations: {},
    messages: [{ version: 'v0.9', updateDataModel: { surfaceId: 'app-1', path: '/', value: { items, draft: {} } } }],
  }) as unknown as Download

const queued = (name: string, context: Record<string, unknown>, record: object, kind: 'insert' | 'update') =>
  ({
    seq: kind === 'insert' ? 2 : 1,
    kind,
    state: 'queued',
    operation: `op-${name}-${kind}`,
    record,
    action: { name, context },
  }) as unknown as OutboxItem

test('a record reads its choice by label, and a change made here changes the label with it', () => {
  const d = copy([
    { id: 1, _revision: 1, description: 'Lunch', kind: 'food', _labels: { kind: 'Food' } },
    { id: 2, _revision: 1, description: 'Bus', kind: 'travel' },
    { id: 3, _revision: 1, description: 'Misc', kind: 'other' },
  ])
  const edit = queued('edit', { id: 1, kind: 'travel' }, { id: 1, revision: 1 }, 'update')
  const made = queued('add', { description: 'Taxi', kind: 'food' }, { id: -1, revision: null }, 'insert')
  const items = itemsOf(d, [edit, made])
  expect(items.map((i) => [i.description, i.kind, (i._labels as Record<string, string>).kind])).toEqual([
    ['Lunch', 'travel', 'Travel'],
    ['Bus', 'travel', 'Travel'],
    ['Misc', 'other', 'other'],
    ['Taxi', 'food', 'Food'],
  ])
})

test('a record with no choice has no labels', () => {
  const [item] = itemsOf(copy([{ id: 1, _revision: 1, description: 'Lunch', _labels: { kind: 'stale' } }]), [])
  expect(item).toEqual({ id: 1, _revision: 1, description: 'Lunch' })
})
