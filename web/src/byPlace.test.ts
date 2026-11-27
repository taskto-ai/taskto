import { expect, test } from 'vitest'
import { byPlace, placeholders } from './byPlace'

test('keys each item by its place, the same on every call', () => {
  expect(byPlace(['a', 'a', 'b'])).toEqual([
    { key: '0', item: 'a', place: 0 },
    { key: '1', item: 'a', place: 1 },
    { key: '2', item: 'b', place: 2 },
  ])
  expect(byPlace(['x', 'y'])).toEqual(byPlace(['x', 'y']))
})

test('makes so many placeholders', () => {
  expect(placeholders(3).map((p) => p.key)).toEqual(['0', '1', '2'])
  expect(placeholders(0)).toEqual([])
})
