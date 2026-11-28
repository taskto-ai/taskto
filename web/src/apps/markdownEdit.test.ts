import { expect, test } from 'vitest'
import {
  CHECKLIST_PREFIX,
  HEADING_PREFIX,
  keepBreaks,
  LIST_PREFIX,
  link,
  normalizeBreaks,
  prefixLines,
  splice,
  wrap,
} from './markdownEdit'

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

test('a line break at the very start does not push the first line out of the edit', () => {
  expect(prefixLines('\nabc', 0, 0, LIST_PREFIX).text).toBe('- \nabc')
})

test('taking a prefix off keeps the caret where it was, and a caret in the prefix goes to the line start', () => {
  expect(prefixLines('- task', 0, 0, LIST_PREFIX)).toEqual({ text: 'task', start: 0, end: 0 })
  expect(prefixLines('- task', 4, 4, LIST_PREFIX)).toEqual({ text: 'task', start: 2, end: 2 })
  expect(prefixLines('- one\n- two', 2, 11, LIST_PREFIX)).toEqual({ text: 'one\ntwo', start: 0, end: 7 })
})

test('adding a prefix moves a caret in the line along with its text', () => {
  expect(prefixLines('task', 2, 2, LIST_PREFIX)).toEqual({ text: '- task', start: 4, end: 4 })
})

test('wrap marks each paragraph of a selection on its own, so each one is bold', () => {
  expect(wrap('one\n\ntwo', 0, 8, '**', 'x').text).toBe('**one**\n\n**two**')
  expect(wrap('one\ntwo', 0, 7, '**', 'x').text).toBe('**one**\n**two**')
  expect(wrap('  one\n\n two ', 0, 12, '**', 'x').text).toBe('  **one**\n\n **two** ')
})

test('wrap over several lines takes the markers off when every line has them, and skips lines that have them', () => {
  expect(wrap('**one**\n\n**two**', 0, 16, '**', 'x').text).toBe('one\n\ntwo')
  expect(wrap('**one**\ntwo', 0, 11, '**', 'x').text).toBe('**one**\n**two**')
})

test('wrap keeps spaces at the ends of the selection outside the markers', () => {
  expect(wrap('a  b  c', 1, 6, '**', 'x')).toEqual({ text: 'a  **b**  c', start: 5, end: 6 })
  expect(wrap('a   c', 1, 4, '**', 'x').text).toBe('a   **x**c')
})

test('wrap uses the in-word marker when a letter or digit touches the selection', () => {
  expect(wrap('ordinary', 2, 5, '_', 'x', '*').text).toBe('or*din*ary')
  expect(wrap('a ordinary', 2, 10, '_', 'x', '*').text).toBe('a _ordinary_')
  expect(wrap('x2y', 1, 2, '_', 'x', '*').text).toBe('x*2*y')
})

test('wrap takes either italic marker off again, and never takes one star off a bold pair', () => {
  expect(wrap('or*din*ary', 3, 6, '_', 'x', '*').text).toBe('ordinary')
  expect(wrap('_one_', 1, 4, '_', 'x', '*').text).toBe('one')
  expect(wrap('**one**', 2, 5, '_', 'x', '*').text).toBe('**_one_**')
  expect(wrap('**one**', 0, 7, '_', 'x', '*').text).toBe('_**one**_')
})

test('splice finds the one range an edit changes', () => {
  expect(splice('plan the week', 'plan the **week**')).toEqual({ at: 9, removed: 4, insert: '**week**' })
  expect(splice('a **b** c', 'a b c')).toEqual({ at: 2, removed: 5, insert: 'b' })
  expect(splice('same', 'same')).toEqual({ at: 4, removed: 0, insert: '' })
})

test('Windows line breaks are one break, and an edit leaves the others as they were', () => {
  expect(normalizeBreaks('a\r\nb\rc\nd')).toBe('a\nb\nc\nd')
  expect(keepBreaks('one\r\ntwo\r\nthree', 'one\n**two**\nthree')).toBe('one\r\n**two**\r\nthree')
  expect(keepBreaks('one\r\ntwo', 'one\r\ntwo'.replace('\r', ''))).toBe('one\r\ntwo')
  expect(keepBreaks('one\r\ntwo', 'onetwo')).toBe('onetwo')
  expect(keepBreaks('one\r\ntwo', 'one\ntwo\nmore')).toBe('one\r\ntwo\nmore')
  expect(keepBreaks('one\ntwo', 'one\ntwo!')).toBe('one\ntwo!')
})
