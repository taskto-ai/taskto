import { expect, test } from 'vitest'
import { CHECKLIST_PREFIX, HEADING_PREFIX, LIST_PREFIX, link, prefixLines, wrap } from './markdownEdit'

test('wrap surrounds the selection and keeps it selected', () => {
  expect(wrap('make it loud', 8, 12, '**', 'x')).toEqual({ text: 'make it **loud**', start: 10, end: 14 })
})

test('wrap with nothing selected inserts the sample and selects it', () => {
  expect(wrap('ab', 1, 1, '_', 'text')).toEqual({ text: 'a_text_b', start: 2, end: 6 })
})

test('wrap takes the marker off again, whether it surrounds the selection or is part of it', () => {
  expect(wrap('make it **loud**', 10, 14, '**', 'x')).toEqual({ text: 'make it loud', start: 8, end: 12 })
  expect(wrap('make it **loud**', 8, 16, '**', 'x')).toEqual({ text: 'make it loud', start: 8, end: 12 })
})

test('wrap at the very start of the text does not read before it', () => {
  expect(wrap('**', 0, 0, '**', 'x')).toEqual({ text: '**x****', start: 2, end: 3 })
})

test('prefixLines puts the prefix on every selected line, and not on the others', () => {
  const text = 'one\ntwo\nthree'
  const edit = prefixLines(text, 2, 6, LIST_PREFIX)
  expect(edit.text).toBe('- one\n- two\nthree')
  expect(edit.start).toBe(4)
  expect(edit.end).toBe(10)
})

test('prefixLines leaves blank lines alone and takes the prefix off when every line has it', () => {
  const added = prefixLines('a\n\nb', 0, 4, LIST_PREFIX)
  expect(added.text).toBe('- a\n\n- b')
  expect(prefixLines(added.text, 0, added.text.length, LIST_PREFIX).text).toBe('a\n\nb')
})

test('a selection that ends just after a newline does not take the next line', () => {
  expect(prefixLines('a\nb', 0, 2, HEADING_PREFIX).text).toBe('### a\nb')
})

test('a heading replaces another heading level, a list replaces a checklist', () => {
  expect(prefixLines('# Title', 0, 0, HEADING_PREFIX).text).toBe('### Title')
  expect(prefixLines('- [ ] task', 0, 0, LIST_PREFIX).text).toBe('- task')
  expect(prefixLines('- task', 0, 0, CHECKLIST_PREFIX).text).toBe('- [ ] task')
  expect(prefixLines('- [x] done', 0, 0, CHECKLIST_PREFIX).text).toBe('done')
})

test('prefixLines on empty text adds the prefix and puts the caret after it', () => {
  expect(prefixLines('', 0, 0, CHECKLIST_PREFIX)).toEqual({ text: '- [ ] ', start: 6, end: 6 })
})

test('link wraps a selection as the label and selects the address', () => {
  const edit = link('see docs now', 4, 8, 'text', 'https://')
  expect(edit.text).toBe('see [docs](https://) now')
  expect(edit.text.slice(edit.start, edit.end)).toBe('https://')
})

test('link with nothing selected makes a sample label and selects it', () => {
  const edit = link('x', 1, 1, 'text', 'https://')
  expect(edit.text).toBe('x[text](https://)')
  expect(edit.text.slice(edit.start, edit.end)).toBe('text')
})
