import { expect, test } from 'vitest'
import { validInputEntry } from './input.js'

test('accepts a bounded taskto input entry', () => {
  expect(validInputEntry({ kind: 'taskto.input', data: { v: 1, author: 7 } })).toBe(true)
})

test('refuses other entry kinds, including pi kinds', () => {
  expect(validInputEntry({ kind: 'other.input', data: {} })).toBe(false)
  expect(validInputEntry({ kind: 'pi.user', data: {} })).toBe(false)
})

test('refuses malformed and oversized entry data', () => {
  expect(validInputEntry({ kind: 'taskto.input', data: null })).toBe(false)
  expect(validInputEntry({ kind: 'taskto.input', data: [] })).toBe(false)
  expect(validInputEntry({ kind: 'taskto.input', data: 'text' })).toBe(false)
  expect(validInputEntry({ kind: 'taskto.input', data: { text: 'x'.repeat(4097) } })).toBe(false)
})
