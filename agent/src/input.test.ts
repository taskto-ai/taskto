import { describe, expect, it, test } from 'vitest'
import { InputError, MAX_NOTE_CHARS, noteEntry, parseNote, validInputEntry } from './input.js'

describe('parseNote', () => {
  it('accepts a note with an author', () => {
    expect(parseNote({ text: 'checked with finance', author: 7 })).toEqual({ text: 'checked with finance', author: 7 })
  })

  it('keeps a request id', () => {
    expect(parseNote({ text: 'a', author: 1, requestId: 'req-1' }).requestId).toBe('req-1')
  })

  it('counts characters, not UTF-16 units', () => {
    expect(() => parseNote({ text: '😀'.repeat(MAX_NOTE_CHARS), author: 1 })).not.toThrow()
    expect(() => parseNote({ text: '😀'.repeat(MAX_NOTE_CHARS + 1), author: 1 })).toThrow(InputError)
  })

  it.each([undefined, null, 5, '', '   \n'])('refuses text %j', (text) => {
    expect(() => parseNote({ text, author: 1 })).toThrow(InputError)
  })

  it.each([undefined, null, 0, -1, 1.5, '1', Number.MAX_SAFE_INTEGER + 1])('refuses author %j', (author) => {
    expect(() => parseNote({ text: 'a', author })).toThrow(InputError)
  })

  it.each([5, 'a'.repeat(129), 'é', 'a\nb'])('refuses request id %j', (requestId) => {
    expect(() => parseNote({ text: 'a', author: 1, requestId })).toThrow(InputError)
  })
})

describe('noteEntry', () => {
  it('has a kind and data and no model, so the model never sees it', () => {
    const entry = noteEntry({ text: 'hello', author: 3 })
    expect(entry).toEqual({ kind: 'taskto.note', data: { v: 1, author: 3, text: 'hello' } })
    expect('model' in entry).toBe(false)
  })
})

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
