import { describe, expect, test, vi } from 'vitest'
import { diffLines, diffWords, type Line, MAX_EDITS, MAX_LINES, splitLines, tokenize } from './diff'

function shown(a: string, b: string) {
  const result = diffLines(a, b)
  if (result.tooLarge) throw new Error('too large')
  return result
}

const marks = (lines: Line[]) => lines.map((l) => `${l.kind === 'add' ? '+' : l.kind === 'del' ? '-' : ' '}${l.text}`)

const rebuilt = (lines: Line[], side: 'a' | 'b') =>
  lines
    .filter((l) => l.kind === 'same' || l.kind === (side === 'a' ? 'del' : 'add'))
    .map((l) => l.text)
    .join('\n')

describe('diffLines', () => {
  test('identical texts have no changes and no hunks', () => {
    const result = shown('one\ntwo\n', 'one\ntwo\n')
    expect(result.added).toBe(0)
    expect(result.removed).toBe(0)
    expect(result.hunks).toEqual([])
    expect(result.lines).toHaveLength(2)
  })

  test('two empty texts are identical', () => {
    const result = shown('', '')
    expect(result.lines).toEqual([])
    expect(result.hunks).toEqual([])
  })

  test('a pure insert shows only added lines, with the new numbers', () => {
    const result = shown('a\nb\n', 'a\nnew\nb\n')
    expect(result.added).toBe(1)
    expect(result.removed).toBe(0)
    expect(marks(result.hunks[0].lines)).toEqual([' a', '+new', ' b'])
    expect(result.lines.find((l) => l.kind === 'add')).toMatchObject({ bLine: 2, aLine: undefined })
  })

  test('a pure delete shows only removed lines, with the old numbers', () => {
    const result = shown('a\ngone\nb\n', 'a\nb\n')
    expect(result.added).toBe(0)
    expect(result.removed).toBe(1)
    expect(marks(result.hunks[0].lines)).toEqual([' a', '-gone', ' b'])
    expect(result.lines.find((l) => l.kind === 'del')).toMatchObject({ aLine: 2, bLine: undefined })
  })

  test('against an empty text every line is added, and the other way every line is removed', () => {
    const made = shown('', 'one\ntwo\n')
    expect([made.added, made.removed]).toEqual([2, 0])
    expect(made.hunks).toHaveLength(1)
    expect(made.hunks[0]).toMatchObject({ aStart: 1, bStart: 1, aCount: 0, bCount: 2 })
    const wiped = shown('one\ntwo\n', '')
    expect([wiped.added, wiped.removed]).toEqual([0, 2])
    expect(wiped.hunks[0]).toMatchObject({ aStart: 1, bStart: 1, aCount: 2, bCount: 0 })
  })

  test('a changed line is its removal followed by its addition', () => {
    const result = shown('a\nold line\nb\n', 'a\nnew line\nb\n')
    expect(marks(result.hunks[0].lines)).toEqual([' a', '-old line', '+new line', ' b'])
  })

  test('several changed lines list the removals first, then the additions', () => {
    const result = shown('x\n1\n2\ny\n', 'x\nA\nB\ny\n')
    expect(marks(result.lines)).toEqual([' x', '-1', '-2', '+A', '+B', ' y'])
  })

  test('a moved block shows as removed in one place and added in the other', () => {
    const result = shown('a\nb\nmoved\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\n', 'a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nmoved\n')
    expect(result.removed).toBe(1)
    expect(result.added).toBe(1)
    expect(result.lines.filter((l) => l.kind === 'del').map((l) => l.text)).toEqual(['moved'])
    expect(result.lines.filter((l) => l.kind === 'add').map((l) => l.text)).toEqual(['moved'])
    expect(result.hunks).toHaveLength(2)
  })

  test('a missing newline at the end is a change', () => {
    const result = shown('a\nb', 'a\nb\n')
    expect(result.removed).toBe(1)
    expect(result.added).toBe(1)
    const [gone, made] = result.lines.filter((l) => l.kind !== 'same')
    expect(gone).toMatchObject({ kind: 'del', text: 'b', noEol: true })
    expect(made).toMatchObject({ kind: 'add', text: 'b', noEol: false })
  })

  test('a trailing newline on both sides is not a line of its own', () => {
    expect(splitLines('a\n')).toEqual(['a'])
    expect(splitLines('a\n\n')).toEqual(['a', ''])
    expect(shown('a\n', 'a\n\n').added).toBe(1)
  })

  test('CRLF and LF lines are equal', () => {
    const result = shown('a\r\nb\r\nc\r\n', 'a\nb\nc\n')
    expect(result.added + result.removed).toBe(0)
    expect(shown('a\r\nb\r\n', 'a\nB\n').removed).toBe(1)
  })

  test('context is three lines, and farther unchanged lines fall between hunks', () => {
    const base = Array.from({ length: 30 }, (_, i) => `line ${i + 1}`)
    const changed = base.map((l, i) => (i === 2 || i === 25 ? `${l} changed` : l))
    const result = shown(`${base.join('\n')}\n`, `${changed.join('\n')}\n`)
    expect(result.hunks).toHaveLength(2)
    expect(result.hunks[0]).toMatchObject({ aStart: 1, bStart: 1, aCount: 6, bCount: 6 })
    expect(result.hunks[1]).toMatchObject({ aStart: 23, bStart: 23, aCount: 7, bCount: 7 })
    const gap = result.lines.slice(result.hunks[0].end, result.hunks[1].start)
    expect(gap).toHaveLength(16)
    expect(gap.every((l) => l.kind === 'same')).toBe(true)
  })

  test('numbers stay right after earlier insertions and deletions', () => {
    const result = shown('a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\n', 'a\nX\nY\nb\nc\nd\ne\nf\ng\nh\ni\nj\nK\n')
    const last = result.lines.find((l) => l.kind === 'add' && l.text === 'K')
    expect(last?.bLine).toBe(13)
    const gone = result.lines.find((l) => l.kind === 'del')
    expect(gone).toMatchObject({ text: 'k', aLine: 11 })
  })

  test('rebuilding either side from the lines gives the text back', () => {
    const a = 'one\ntwo\nthree\nfour\nfive\n'
    const b = 'one\n2\nthree\nfive\nsix\n'
    const result = shown(a, b)
    expect(rebuilt(result.lines, 'a')).toBe('one\ntwo\nthree\nfour\nfive')
    expect(rebuilt(result.lines, 'b')).toBe('one\n2\nthree\nfive\nsix')
  })

  test('shortest edit script on mixed edits', () => {
    const a = 'A\nB\nC\nA\nB\nB\nA\n'
    const b = 'C\nB\nA\nB\nA\nC\n'
    const result = shown(a, b)
    expect(result.added + result.removed).toBe(5)
    expect(rebuilt(result.lines, 'a')).toBe('A\nB\nC\nA\nB\nB\nA')
    expect(rebuilt(result.lines, 'b')).toBe('C\nB\nA\nB\nA\nC')
  })

  test('a side over the line limit is too large, without comparing', () => {
    const big = `${'x\n'.repeat(MAX_LINES + 1)}`
    expect(diffLines(big, big)).toEqual({ tooLarge: true })
    expect(diffLines('', big)).toEqual({ tooLarge: true })
    expect(diffLines(big, '')).toEqual({ tooLarge: true })
  })

  test('a distance over the edit limit is too large, and a big but small change is not', () => {
    const a = Array.from({ length: MAX_EDITS }, (_, i) => `a${i}`).join('\n')
    const b = Array.from({ length: MAX_EDITS }, (_, i) => `b${i}`).join('\n')
    expect(diffLines(a, b)).toEqual({ tooLarge: true })
    const long = Array.from({ length: MAX_LINES }, (_, i) => `line ${i}`)
    const edited = [...long]
    edited[10000] = 'changed'
    const result = shown(`${long.join('\n')}\n`, `${edited.join('\n')}\n`)
    expect([result.added, result.removed]).toEqual([1, 1])
  })

  test('a large change below the limits finishes', () => {
    const a = Array.from({ length: 1500 }, (_, i) => `a${i}`).join('\n')
    const b = Array.from({ length: 1500 }, (_, i) => `b${i}`).join('\n')
    const result = shown(a, b)
    expect([result.added, result.removed]).toEqual([1500, 1500])
  })

  test('accents, emoji and CJK lines compare as text', () => {
    const result = shown('café\n日本語\n🎉 party\n', 'café\n日本\n🎉 party\n')
    expect(marks(result.lines)).toEqual([' café', '-日本語', '+日本', ' 🎉 party'])
  })
})

const join = (parts: { text: string }[]) => parts.map((p) => p.text).join('')
const changed = (parts: { text: string; changed: boolean }[]) => parts.filter((p) => p.changed).map((p) => p.text)

describe('diffWords', () => {
  test('marks the words that differ on each side and keeps the text whole', () => {
    const words = diffWords('The quick brown fox', 'The slow brown fox')
    expect(words).not.toBeNull()
    expect(join(words?.old ?? [])).toBe('The quick brown fox')
    expect(join(words?.new ?? [])).toBe('The slow brown fox')
    expect(changed(words?.old ?? [])).toEqual(['quick'])
    expect(changed(words?.new ?? [])).toEqual(['slow'])
  })

  test('punctuation and spaces are tokens of their own', () => {
    const words = diffWords('Hello, world.', 'Hello; world.')
    expect(changed(words?.old ?? [])).toEqual([','])
    expect(changed(words?.new ?? [])).toEqual([';'])
  })

  test('an added word marks only the new side', () => {
    const words = diffWords('one three', 'one two three')
    expect(changed(words?.old ?? [])).toEqual([])
    expect(join(words?.new ?? [])).toBe('one two three')
    expect(changed(words?.new ?? []).join('')).toContain('two')
  })

  test('accents, emoji and CJK words are kept whole', () => {
    const accent = diffWords('un café noir', 'un cafe noir')
    expect(changed(accent?.old ?? [])).toEqual(['café'])
    expect(changed(accent?.new ?? [])).toEqual(['cafe'])
    const emoji = diffWords('great 🎉 job', 'great 🚀 job')
    expect(changed(emoji?.old ?? [])).toEqual(['🎉'])
    expect(changed(emoji?.new ?? [])).toEqual(['🚀'])
    const cjk = diffWords('今日は天気です', '今日は雨です')
    expect(join(cjk?.old ?? [])).toBe('今日は天気です')
    expect(join(cjk?.new ?? [])).toBe('今日は雨です')
    expect(changed(cjk?.old ?? []).length).toBeGreaterThan(0)
    expect(changed(cjk?.new ?? []).length).toBeGreaterThan(0)
  })

  test('a line with too many words is left without highlights', () => {
    const long = Array.from({ length: 4001 }, (_, i) => `w${i}`).join(' ')
    expect(diffWords(long, 'short')).toBeNull()
  })
})

describe('the limits, and how words are cut and joined', () => {
  test('the line limit is 20,000 lines on a side, and the edit limit 5,000 changed lines', () => {
    const lines = (n: number, prefix: string) => Array.from({ length: n }, (_, i) => `${prefix}${i}`).join('\n')
    expect(diffLines(`${lines(20000, 'x')}\n`, `${lines(20000, 'x')}\n`).tooLarge).toBe(false)
    expect(diffLines(`${lines(20001, 'x')}\n`, `${lines(20001, 'x')}\n`)).toEqual({ tooLarge: true })
    expect(diffLines(lines(2500, 'a'), lines(2500, 'b')).tooLarge).toBe(false)
    expect(diffLines(lines(2501, 'a'), lines(2501, 'b'))).toEqual({ tooLarge: true })
  })

  test('words are limited to 4,000 tokens on a side and 1,000 edits, however small the other side is', () => {
    const same = Array.from({ length: 3000 }, (_, i) => `w${i}`).join(' ')
    expect(diffWords(same, `${same} end`)).toBeNull()
    const few = Array.from({ length: 600 }, (_, i) => `a${i}`).join(' ')
    expect(diffWords(few, `${few} end`)).not.toBeNull()
    const left = Array.from({ length: 800 }, (_, i) => `a${i}`).join(' ')
    const right = Array.from({ length: 800 }, (_, i) => `b${i}`).join(' ')
    expect(diffWords(left, right)).toBeNull()
  })

  test('neighbouring words of one kind come back as one part', () => {
    expect(diffWords('the quick brown fox', 'the slow brown fox')).toEqual({
      old: [
        { text: 'the ', changed: false },
        { text: 'quick', changed: true },
        { text: ' brown fox', changed: false },
      ],
      new: [
        { text: 'the ', changed: false },
        { text: 'slow', changed: true },
        { text: ' brown fox', changed: false },
      ],
    })
  })

  test('without a word segmenter, text is cut into words, spaces and punctuation', () => {
    const intl = new Proxy(Intl, { has: (target, key) => key !== 'Segmenter' && key in target })
    vi.stubGlobal('Intl', intl)
    try {
      expect(tokenize('héllo, wörld 你好')).toEqual(['héllo', ',', ' ', 'wörld', ' ', '你好'])
    } finally {
      vi.unstubAllGlobals()
    }
  })

  test('a NUL in a line is content, never the no-newline marker', () => {
    const same = diffLines('a', 'a\u0000\n')
    expect(same.tooLarge).toBe(false)
    if (same.tooLarge) return
    expect([same.added, same.removed]).toEqual([1, 1])
    const shown = diffLines('', 'a\u0000\n')
    expect(shown.tooLarge).toBe(false)
    if (shown.tooLarge) return
    expect(shown.lines.map((l) => ({ text: l.text, noEol: l.noEol }))).toEqual([{ text: 'a\u0000', noEol: false }])
  })

  test('a 5,000-token line consumes at most 4,001 tokens, with and without a word segmenter', () => {
    const consumed: number[] = []
    class CountingSegmenter {
      segment(text: string) {
        const parts = text.split(/(?<=\s)|(?=\s)/)
        const at = consumed.push(0) - 1
        return {
          *[Symbol.iterator]() {
            for (const part of parts) {
              consumed[at]++
              yield { segment: part }
            }
          },
        }
      }
    }
    const line = Array.from({ length: 5000 }, (_, i) => `w${i}`).join(' ')
    vi.stubGlobal('Intl', { ...Intl, Segmenter: CountingSegmenter })
    try {
      expect(diffWords(line, 'x')).toBeNull()
      expect(Math.max(...consumed)).toBeLessThanOrEqual(4001)
      expect(tokenize(line, 4001)).toHaveLength(4001)
    } finally {
      vi.unstubAllGlobals()
    }
    const intl = new Proxy(Intl, { has: (target, key) => key !== 'Segmenter' && key in target })
    vi.stubGlobal('Intl', intl)
    try {
      expect(tokenize(line, 4001)).toHaveLength(4001)
    } finally {
      vi.unstubAllGlobals()
    }
  })
})
