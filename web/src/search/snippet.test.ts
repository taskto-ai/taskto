import { isMainThread } from 'node:worker_threads'
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
// with the code right. Each input is cleaned at three sizes, in two stages, and the time of one
// size is compared with the time of the one before it. Linear work takes about as many times as
// long as the size grew (8 times, in each stage); quadratic work takes the square of that, 64
// times. A quadratic step with a small constant (a native search for a closing bracket from each
// opening one) hides under the linear work at the first sizes and takes over only at the last,
// where the old test's 300,000 repeats caught it; so the second stage goes up to 320,000.
//
// What is measured is the process's CPU time, not the wall clock: on a machine with more work
// than cores a short run can find a quiet moment while a long one is paused again and again,
// which makes the long one look too slow. That relies on the process being this test file's
// alone (Vitest's default pool, `forks`: a process per file, checked below): CPU time counts
// every thread of the process, the compiler's and the garbage collector's too. A time below
// FLOOR_MS counts as FLOOR_MS, so for an input whose smallest size is that quick, stage one is
// really a bound of 40 × FLOOR_MS = 80 ms of CPU for 40,000 repeats.
//
// Each size is timed a few times after a warm-up and the fastest counts. A ratio over the limit
// is measured again, but only while the larger run is short (under RETRY_MS), where a pause can
// explain it; a run of seconds over the limit is a cleaner that is not linear, and it fails at
// once, as does every input after it, so a regression is reported in seconds.
const NASTY = ['[', '(', '*', '_', '`', '~', '](', '[](', '*a', '_a_', '`a', '![', '>', '- ']
const SMALL = 5_000
const MID = 40_000
const BIG = 320_000
// Linear is 8 and quadratic is 64 or more. Stage one is far from both; measured, an unmutated
// cleaner is at most 11.4 here. Stage two: an unmutated cleaner is between 2 and 23.3 (the most
// is for "- ", whose result is the biggest), and a cleaner that is quadratic in a small way
// between 44 and 59; 30 is between them, and a ratio over it gets more attempts.
const MAX_RATIO = { first: 40, second: 30 }
// Below this, the timer's own noise is most of a run.
const FLOOR_MS = 2
// A run of CPU time longer than this is not noise (a linear cleaner takes at most a fifth of
// it at 320,000 repeats): it stops the measuring, and fails if the ratio is over the limit.
const RETRY_MS = 1000
const ATTEMPTS = 3

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
// RETRY_MS, which is then the answer's lower bound, however many more runs there would be.
function fastest(fn: () => void, runs = 3): number {
  let best = Number.POSITIVE_INFINITY
  for (let i = 0; i < runs; i++) {
    const started = process.cpuUsage()
    fn()
    const used = process.cpuUsage(started)
    const took = (used.user + used.system) / 1000
    best = Math.min(best, took)
    if (took > RETRY_MS) break
  }
  return best
}

type Growth = { ok: boolean; decisive: boolean; message: string }

// Whether the time of large is less than max times the time of small.
function grows(small: () => void, large: () => void, max: number, sizes: string): Growth {
  let message = ''
  for (let attempt = 0; attempt < ATTEMPTS; attempt++) {
    const smallMs = fastest(small)
    if (smallMs > RETRY_MS)
      return { ok: false, decisive: true, message: `${sizes}: ${smallMs.toFixed(0)} ms for the smaller` }
    const largeMs = fastest(large)
    const ratio = largeMs / Math.max(smallMs, FLOOR_MS)
    message = `${sizes}: ${smallMs.toFixed(1)} ms and ${largeMs.toFixed(1)} ms, ${ratio.toFixed(1)} times, at most ${max}`
    if (ratio < max) return { ok: true, decisive: false, message }
    if (largeMs > RETRY_MS) return { ok: false, decisive: true, message }
  }
  return { ok: false, decisive: false, message }
}

// The input that failed beyond doubt: the others would only take longer to say the same.
let decisiveFailure = ''

test('the cleaner runs in a process of its own, which its timing relies on', () => {
  expect(isMainThread).toBe(true)
})

test.for(NASTY)(
  'a snippet of repeated %j between two words is cleaned in linear time',
  // Vitest can't interrupt code that doesn't wait, so this bounds only a run that ends: what
  // stops a cleaner that is far too slow is RETRY_MS above. It is long because a busy machine
  // stretches the wall time of a test that is quick in CPU time.
  { timeout: 60_000 },
  (unit, { skip }) => {
    if (decisiveFailure !== '') skip(`not linear for ${decisiveFailure}`)
    const whole = { limit: Number.POSITIVE_INFINITY }
    const small = snippetOf(unit, SMALL)
    const mid = snippetOf(unit, MID)
    const big = snippetOf(unit, BIG)
    // The scanner itself, on all of it (no window).
    cleanSnippet(snippetOf(unit, SMALL / 10), whole) // warm-up: the first run pays for compiling

    const first = grows(
      () => cleanSnippet(small, whole),
      () => cleanSnippet(mid, whole),
      MAX_RATIO.first,
      `${unit} ${SMALL} to ${MID} repeats`,
    )
    if (first.decisive) decisiveFailure = JSON.stringify(unit)
    expect(first.ok, first.message).toBe(true)
    expect(marked(cleanSnippet(mid, whole))).toEqual(['orchard'])

    // The last stage, only for a cleaner that passed the first.
    const second = grows(
      () => cleanSnippet(mid, whole),
      () => cleanSnippet(big, whole),
      MAX_RATIO.second,
      `${unit} ${MID} to ${BIG} repeats`,
    )
    if (second.decisive) decisiveFailure = JSON.stringify(unit)
    expect(second.ok, second.message).toBe(true)

    // The window: by default only a piece around the first match is worked on, so the same input
    // costs a fraction of all of it, and shows the match.
    const wholeMs = fastest(() => cleanSnippet(mid, whole))
    const windowMs = fastest(() => cleanSnippet(mid))
    expect(windowMs).toBeLessThan(Math.max(wholeMs / 2, FLOOR_MS))
    const bounded = cleanSnippet(mid)
    expect(marked(bounded)).toEqual(['orchard'])
    expect(plain(bounded).length).toBeLessThanOrEqual(1202)
  },
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
