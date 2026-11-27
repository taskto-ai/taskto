// @vitest-environment jsdom

import { expect, test } from 'vitest'
import { ApiError, errorText } from '../api'
import { fieldLabels } from './labels'

type Component = { type: string; properties: Record<string, unknown> }

function screens(): [string, Component][] {
  const c = (id: string, type: string, properties: Record<string, unknown>): [string, Component] => [
    id,
    { type, properties },
  ]
  const submit = (context: Record<string, unknown>) => ({ action: { event: { name: 'add', context } } })
  return [
    c('root', 'Screens', {
      current: { path: '/screen' },
      screens: [
        { name: 'new', child: 'new-root' },
        { name: 'other', child: 'other-root' },
      ],
    }),
    c('new-root', 'Column', { children: ['hidden-note', 'note-when', 'add'] }),
    c('hidden-note', 'TextField', { label: 'Note (hidden)', value: { path: '/draft/hidden' } }),
    c('note-when', 'Show', { when: { field: 'category', op: 'eq', value: 'other' }, child: 'note' }),
    c('note', 'TextField', { label: 'What kind?', value: { path: '/draft/note' } }),
    c('add', 'Button', submit({ note: { path: '/draft/note' }, hidden: { path: '/draft/hidden' } })),
    c('other-root', 'Column', { children: ['elsewhere', 'add-other'] }),
    c('elsewhere', 'TextField', { label: 'On another screen', value: { path: '/draft/note' } }),
    c('add-other', 'Button', submit({ note: { path: '/draft/note' } })),
  ]
}

const reading = (data: Record<string, unknown>) => (path: string) => data[path]

test('a label is one the person can see: on the open screen and not hidden by a Show', () => {
  const hidden = fieldLabels(screens(), reading({ '/screen': 'new', '/draft': { category: 'home' } }))
  expect(hidden.get('note')).toBeUndefined()
  expect(hidden.get('hidden')).toBe('Note (hidden)')
  const shown = fieldLabels(screens(), reading({ '/screen': 'new', '/draft': { category: 'other' } }))
  expect(shown.get('note')).toBe('What kind?')
  const other = fieldLabels(screens(), reading({ '/screen': 'other', '/draft': {} }))
  expect(other.get('note')).toBe('On another screen')
  expect(other.get('hidden')).toBeUndefined()
})

test('an error about a field with no label names the field, even one that is an object’s own method', () => {
  const labels = fieldLabels(screens(), reading({ '/screen': 'new', '/draft': {} }))
  const refused = (field: string) =>
    new ApiError(422, 'x', { error: 'x', code: 'app_field_required', params: { field } })
  expect(errorText(refused('constructor'), labels)).toBe('constructor is required.')
  expect(errorText(refused('__proto__'), labels)).toBe('__proto__ is required.')
  expect(errorText(refused('hidden'), labels)).toBe('Note (hidden) is required.')
})
