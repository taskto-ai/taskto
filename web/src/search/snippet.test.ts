import { expect, test } from 'vitest'
import { cleanSnippet, type SnippetPiece } from './snippet'

const plain = (pieces: SnippetPiece[]) => pieces.map((p) => p.text).join('')
const marked = (pieces: SnippetPiece[]) => pieces.filter((p) => p.match).map((p) => p.text)

test('plain text is left as it is, with its matches', () => {
  const pieces = [{ text: '…we grow the ' }, { text: 'orchard', match: true }, { text: ' next year…' }]
  expect(cleanSnippet(pieces)).toEqual(pieces)
})

test("a heading's #, bold, emphasis, code and strikethrough marks are left out", () => {
  const pieces = cleanSnippet([
    { text: '# Plan\n\nThe **first ' },
    { text: 'orchard', match: true },
    { text: '** is *small*, `code` and ~~gone~~ and snake_case_name.' },
  ])
  expect(plain(pieces)).toBe('Plan The first orchard is small, code and gone and snake_case_name.')
  expect(marked(pieces)).toEqual(['orchard'])
})

test('marks around a match, and a mark the snippet was cut in the middle of, go too', () => {
  expect(plain(cleanSnippet([{ text: '**' }, { text: 'orchard', match: true }, { text: '**' }]))).toBe('orchard')
  expect(marked(cleanSnippet([{ text: '**' }, { text: 'orchard', match: true }, { text: '**' }]))).toEqual(['orchard'])
  // Cut after the opening of a bold run.
  expect(plain(cleanSnippet([{ text: 'a **b ' }, { text: 'c', match: true }]))).toBe('a b c')
})

test('links and images show their words, lists and quotes lose their markers, lines run together', () => {
  const text = [
    '## Notes',
    '- see [the plan](notes/plan.md) and ![a map](map.png)',
    '- [ ] ship it',
    '> a quote',
    '```ts',
    'const x = 1',
    '```',
  ].join('\n')
  expect(plain(cleanSnippet([{ text }]))).toBe('Notes see the plan and a map ship it a quote const x = 1')
})

test('a # inside a line, and HTML, stay as written; nothing becomes markup', () => {
  const pieces = cleanSnippet([{ text: 'C# and #tag and <b>next</b> year' }])
  expect(plain(pieces)).toBe('C# and #tag and <b>next</b> year')
})

test('a snippet with nothing left shows nothing', () => {
  expect(cleanSnippet([{ text: '#  \n' }])).toEqual([])
  expect(cleanSnippet([])).toEqual([])
})
