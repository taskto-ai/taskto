import { expect, test } from 'vitest'
import { cleanSnippet, isMarkdownPath, type SnippetPiece } from './snippet'

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

test("a match inside a link's destination is never removed: the destination stays, after the link's words", () => {
  const pieces = cleanSnippet([{ text: 'see [the guide](docs/' }, { text: 'setup', match: true }, { text: '.md) now' }])
  expect(marked(pieces)).toEqual(['setup'])
  expect(plain(pieces)).toBe('see the guide (docs/setup.md) now')
  // The same for an image, and for a match in the words, which always stay.
  expect(marked(cleanSnippet([{ text: '![a ' }, { text: 'map', match: true }, { text: '](m.png)' }]))).toEqual(['map'])
})

test('a highlight that spans a formatting mark is kept whole, and a mark whose partner would stay stays too', () => {
  // A phrase match covers the bold run's opening mark.
  const spans = cleanSnippet([{ text: 'a ' }, { text: 'red **apple', match: true }, { text: '** pie' }])
  expect(marked(spans)).toEqual(['red **apple'])
  expect(plain(spans)).toBe('a red **apple** pie')
  // A match across the words of bold text: the marks around go.
  const across = cleanSnippet([{ text: 'a **' }, { text: 'red apple', match: true }, { text: '** pie' }])
  expect(plain(across)).toBe('a red apple pie')
  expect(marked(across)).toEqual(['red apple'])
  // A match across a link's closing bracket.
  const link = cleanSnippet([{ text: 'see [the ' }, { text: 'plan](notes/pl', match: true }, { text: 'an.md) now' }])
  expect(marked(link)).toEqual(['plan](notes/pl'])
})

test("a file that is not Markdown is shown literally: an upload's name keeps its underscores", () => {
  const pieces = [{ text: 'my__file__name' }, { text: '.pdf', match: true }]
  expect(cleanSnippet(pieces, { markdown: false })).toEqual(pieces)
  expect(plain(cleanSnippet([{ text: '# not a heading **x** `y` [a](b)' }], { markdown: false }))).toBe(
    '# not a heading **x** `y` [a](b)',
  )
  // And Markdown's own rule for words: marks inside a word are text.
  expect(plain(cleanSnippet([{ text: 'my__file__name.pdf and a*b*c and snake_case_' }]))).toBe(
    'my__file__name.pdf and a*b*c and snake_case_',
  )
  expect(isMarkdownPath('notes/Plan.MD')).toBe(true)
  expect(isMarkdownPath('scan.pdf')).toBe(false)
  expect(isMarkdownPath('notes.md.txt')).toBe(false)
})

test('nothing inside inline code or a code block is formatting', () => {
  expect(plain(cleanSnippet([{ text: 'use `a ** b` and `*x*` or ``[l](u)`` here' }]))).toBe(
    'use a ** b and *x* or [l](u) here',
  )
  const block = ['```md', '# not a heading', '**bold** and [l](u)', '- item', '```', 'after'].join('\n')
  expect(plain(cleanSnippet([{ text: block }]))).toBe('# not a heading **bold** and [l](u) - item after')
  // Backticks that are never closed are text.
  expect(plain(cleanSnippet([{ text: 'one ` tick and ``two' }]))).toBe('one ` tick and ``two')
})

test('a snippet is cut to a window around its first match, which survives the cut', () => {
  const filler = 'lorem ipsum '.repeat(400)
  const pieces = cleanSnippet([{ text: filler }, { text: 'orchard', match: true }, { text: filler }])
  expect(marked(pieces)).toEqual(['orchard'])
  const shown = plain(pieces)
  expect(shown.length).toBeLessThanOrEqual(1202)
  expect(shown.startsWith('…') && shown.endsWith('…')).toBe(true)
  // A match longer than the window is cut, and is still the highlight.
  const long = cleanSnippet([{ text: 'x'.repeat(5000), match: true }])
  expect(marked(long)).toHaveLength(1)
  expect(marked(long)[0].length).toBeGreaterThan(1000)
  // The window doesn't split a character made of two units.
  const wide = cleanSnippet([{ text: '😀'.repeat(2000) }])
  expect(plain(wide)).not.toMatch(/[\ud800-\udbff](?![\udc00-\udfff])/)
})

// SQLite's FTS5 limits a snippet by tokens, not characters: brackets are no tokens, so a snippet
// can be as long as the document. A cleaner that backtracks takes time that grows with the
// square of that length, and once froze the page.
//
// The test shows the work is linear, not that it is quick: a clock bound fails on a busy machine
// with the code right. Each input is cleaned at one size and at eight times it; linear work takes
// about eight times as long, quadratic work sixty-four times or more, and the limit is between
// them. What is measured is the process's CPU time, not the wall clock: on a machine with more
// work than cores a short run can find a quiet moment while a long one is paused again and
// again, which makes the long one look too slow, and the test's own time limit is long for the
// same reason. Each size is timed a few times after a warm-up, the fastest counts, and a ratio
// over the limit is measured again before it fails; a quadratic cleaner is over it every time.
const NASTY = ['[', '(', '*', '_', '`', '~', '](', '[](', '*a', '_a_', '`a', '![', '>', '- ']
const SMALL = 5_000
const GROWTH = 8
// Linear is 8 and quadratic is 64 or more; this is between them with room for noise: on a machine
// with far more work than cores the larger run, which allocates more, took up to three times
// as long as it should.
const MAX_RATIO = 40
// Below this, the timer's own noise is most of a run.
const FLOOR_MS = 2
// A very loose backstop, for a run that is far too slow to measure again: it only keeps a
// quadratic cleaner from taking minutes to fail. A linear one is over a hundred times quicker.
const GIVE_UP_MS = 1000

const snippetOf = (unit: string, times: number) => {
  const body = unit.repeat(times)
  return [
    { text: 'first ' },
    { text: body },
    { text: ' ' },
    { text: 'orchard', match: true },
    { text: ` ${body} last` },
  ]
}

// The fastest of a few runs of fn, in milliseconds of CPU time; it stops at once for a run over
// giveUp, which is then the answer's lower bound, however many more runs there would be.
function fastest(fn: () => void, giveUp: number, runs = 3): number {
  let best = Number.POSITIVE_INFINITY
  for (let i = 0; i < runs; i++) {
    const started = process.cpuUsage()
    fn()
    const used = process.cpuUsage(started)
    const took = (used.user + used.system) / 1000
    best = Math.min(best, took)
    if (took > giveUp) break
  }
  return best
}

test.each(NASTY)(
  'a snippet of repeated %j between two words is cleaned in linear time',
  (unit) => {
    const small = snippetOf(unit, SMALL)
    const large = snippetOf(unit, SMALL * GROWTH)
    const whole = { limit: Number.POSITIVE_INFINITY }

    // The scanner itself, on all of it (no window).
    cleanSnippet(snippetOf(unit, SMALL / 10), whole) // warm-up: the first run pays for compiling
    let ratio = 0
    let smallMs = 0
    let largeMs = 0
    for (let attempt = 0; attempt < 3; attempt++) {
      smallMs = fastest(() => cleanSnippet(small, whole), GIVE_UP_MS)
      if (smallMs > GIVE_UP_MS) {
        ratio = Number.POSITIVE_INFINITY // the larger would take sixty-four times as long
        break
      }
      const base = Math.max(smallMs, FLOOR_MS)
      largeMs = fastest(() => cleanSnippet(large, whole), base * MAX_RATIO * 2)
      ratio = largeMs / base
      // Under the limit it passes, and far over it the cleaner is quadratic: measuring again
      // would only take longer. Just over it may be a pause that no run escaped.
      if (ratio < MAX_RATIO || ratio > MAX_RATIO * 2) break
    }
    expect(
      ratio,
      `${smallMs.toFixed(1)} ms for ${SMALL} repeats, ${largeMs.toFixed(1)} ms for ${SMALL * GROWTH}`,
    ).toBeLessThan(MAX_RATIO)
    expect(marked(cleanSnippet(large, whole))).toEqual(['orchard'])

    // The window: by default only a piece around the first match is worked on, so the same input
    // costs a fraction of all of it, and shows the match.
    const windowMs = fastest(() => cleanSnippet(large), GIVE_UP_MS)
    expect(windowMs).toBeLessThan(Math.max(largeMs / 2, FLOOR_MS))
    const bounded = cleanSnippet(large)
    expect(marked(bounded)).toEqual(['orchard'])
    expect(plain(bounded).length).toBeLessThanOrEqual(1202)
    // What is measured is CPU time, which a busy machine doesn't stretch; its wall time it does.
  },
  60_000,
)

test('code keeps its text, and a highlight at the ends of an identifier does not make its underscores marks', () => {
  expect(plain(cleanSnippet([{ text: 'Use `**kwargs` here' }]))).toBe('Use **kwargs here')
  expect(plain(cleanSnippet([{ text: '```py\nx = 2 ** 3\n```' }]))).toBe('x = 2 ** 3')
  const id = cleanSnippet([{ text: 'first', match: true }, { text: '_name_' }, { text: 'last', match: true }])
  expect(plain(id)).toBe('first_name_last')
  expect(marked(id)).toEqual(['first', 'last'])
})

test('a link destination may hold balanced parentheses, and a backslash escapes a mark', () => {
  const link = cleanSnippet([
    { text: 'See [guide](docs/(draft)/' },
    { text: 'plan', match: true },
    { text: '.md) now' },
  ])
  expect(plain(link)).toBe('See guide (docs/(draft)/plan.md) now')
  expect(marked(link)).toEqual(['plan'])
  expect(plain(cleanSnippet([{ text: 'See [guide](docs/(draft)/plan.md) now' }]))).toBe('See guide now')

  expect(plain(cleanSnippet([{ text: 'Keep \\*literal\\* near ' }, { text: 'orchard', match: true }]))).toBe(
    'Keep *literal* near orchard',
  )
  // An escaped backslash is one backslash, and the mark after it is a mark.
  expect(plain(cleanSnippet([{ text: 'a \\\\ and *b*, \\[x\\](y) and \\`z\\`' }]))).toBe('a \\ and b, [x](y) and `z`')
})
