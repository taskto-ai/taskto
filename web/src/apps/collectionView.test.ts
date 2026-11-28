import { expect, test } from 'vitest'
import { readItems, sortItems } from './collectionView'

const ids = (rows: { id: number }[]) => rows.map((r) => r.id)

test('only rows with a whole, non-zero id are items', () => {
  const rows = [{ id: 1 }, { id: 0 }, { id: 1.5 }, { id: '2' }, { name: 'x' }, null, 7, 'x', { id: -3 }]
  expect(ids(readItems(rows))).toEqual([1, -3])
  expect(readItems(undefined)).toEqual([])
  expect(readItems({ id: 1 })).toEqual([])
})

test('numbers sort by value, in both directions', () => {
  const rows = [
    { id: 1, n: 1.5 },
    { id: 2, n: 1.25 },
    { id: 3, n: -4 },
    { id: 4, n: 10 },
  ]
  expect(ids(sortItems(rows, 'n', 'number', 'asc', 'en'))).toEqual([3, 2, 1, 4])
  expect(ids(sortItems(rows, 'n', 'number', 'desc', 'en'))).toEqual([4, 1, 2, 3])
})

test('a choice sorts by the label people see, not by its stored value', () => {
  const rows = [
    { id: 1, c: 'a', _labels: { c: 'Zebra' } },
    { id: 2, c: 'b', _labels: { c: 'Apple' } },
  ]
  expect(ids(sortItems(rows, 'c', 'choice', 'asc', 'en'))).toEqual([2, 1])
  const three = [
    { id: 1, c: 'a', _labels: { c: 'Charlie' } },
    { id: 2, c: 'b', _labels: { c: 'Alpha' } },
    { id: 3, c: 'c', _labels: { c: 'Bravo' } },
  ]
  expect(ids(sortItems(three, 'c', 'choice', 'asc', 'en'))).toEqual([2, 3, 1])
  expect(ids(sortItems([...three].reverse(), 'c', 'choice', 'asc', 'en'))).toEqual([2, 3, 1])
  expect(ids(sortItems(three, 'c', 'choice', 'desc', 'en'))).toEqual([1, 3, 2])
})

test('rows that tie keep the order of their ids, whatever order they arrive in', () => {
  const rows = [
    { id: 3, s: 'same' },
    { id: 1, s: 'same' },
    { id: 2, s: 'same' },
  ]
  expect(ids(sortItems(rows, 's', 'string', 'asc', 'en'))).toEqual([1, 2, 3])
  expect(ids(sortItems(rows, 's', 'string', 'desc', 'en'))).toEqual([1, 2, 3])
})

test('rows with no value keep the order of their ids too', () => {
  const rows = [{ id: 3 }, { id: 1, s: '' }, { id: 2, s: null }]
  expect(ids(sortItems(rows, 's', 'string', 'asc', 'en'))).toEqual([1, 2, 3])
})

test('with no column to sort by the rows are in the order of their ids', () => {
  expect(ids(sortItems([{ id: 3 }, { id: 1 }, { id: 2 }], undefined, undefined, 'asc', 'en'))).toEqual([1, 2, 3])
})

test('rows with no value come last in both directions, and among equal rows those not yet sent come after the others', () => {
  const rows = [{ id: -1, s: 'a' }, { id: 1, s: '' }, { id: 2, s: 'b' }, { id: 3 }, { id: 4, s: 'a' }]
  expect(ids(sortItems(rows, 's', 'string', 'asc', 'en'))).toEqual([4, -1, 2, 1, 3])
  expect(ids(sortItems(rows, 's', 'string', 'desc', 'en'))).toEqual([2, 4, -1, 1, 3])
})
