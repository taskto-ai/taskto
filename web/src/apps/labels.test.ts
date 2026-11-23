// @vitest-environment jsdom

import { expect, test } from 'vitest'
import { ApiError, errorText } from '../api'
import { fieldLabels } from './labels'

type Component = { type: string; properties: Record<string, unknown> }

// A form of two screens: "other" has an input for the same field, and a Show hides one of the
// inputs of "new" until the category is "other".
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
  // The Show's rule doesn't hold: the input behind it is not on the screen.
  const hidden = fieldLabels(screens(), reading({ '/screen': 'new', '/draft': { category: 'home' } }))
  expect(hidden.get('note')).toBeUndefined()
  expect(hidden.get('hidden')).toBe('Note (hidden)')
  // It holds: that input is the field's label, not the one of the screen that isn't open.
  const shown = fieldLabels(screens(), reading({ '/screen': 'new', '/draft': { category: 'other' } }))
  expect(shown.get('note')).toBe('What kind?')
  // Another screen's inputs are not this one's.
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
