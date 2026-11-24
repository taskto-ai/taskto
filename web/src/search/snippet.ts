// A search result's snippet is a piece of a document's source: the server returns it as the
// file has it, in pieces that say which are the matched words. For display the Markdown marks
// (a heading's #, **bold**, `code`, a link's brackets) are noise, so they are taken out here.
// Nothing about what the server returns changes, and nothing here makes markup: the result is
// still plain text in pieces.
//
// Rules the cleaning keeps:
// - A matched character is never removed. A mark that would go but holds a match stays, so the
//   result always shows what the server highlighted (the link's destination, when the match is
//   in it, is shown after the link's words).
// - Only a Markdown document's snippet is cleaned. Any other file's (an upload's path or title)
//   is shown as it is.
// - Nothing inside a code block or an inline code span is a mark, and underscores and asterisks
//   inside a word (snake_case_name, my__file__name) are text.
// - The work is linear in the snippet's length whatever it holds: one pass, with each opening
//   mark looked up in tables made beforehand, never a pattern that backtracks. FTS5 limits a
//   snippet by tokens, not characters, so a snippet can be as long as a document; the text it
//   works on is cut to a window around the first match first.

export type SnippetPiece = { text: string; match?: boolean }

export type SnippetOptions = {
  // Whether the document is Markdown. Default: yes.
  markdown?: boolean
  // The most characters worked on (UTF-16 units). Default: LIMIT.
  limit?: number
}

// What a Markdown document is, as the server says it (knowledge.IsMarkdown): by its extension.
export const isMarkdownPath = (path: string) => /\.md$/i.test(path)

// A result is a few lines of running text: this much is more than it ever shows.
export const LIMIT = 1200
// Of that, how much comes before the first match.
const BEFORE = 300

const ELLIPSIS = '…'

export function cleanSnippet(pieces: SnippetPiece[], options: SnippetOptions = {}): SnippetPiece[] {
  const { text, matched } = bound(pieces, options.limit ?? LIMIT)
  if (options.markdown === false) return group(text, matched, new Uint8Array(text.length).fill(1), new Map())
  return clean(text, matched)
}

// The snippet's text and, for each character, whether it is matched; cut to a window of at most
// limit characters that begins a little before the first match, so the match is in it.
function bound(pieces: SnippetPiece[], limit: number): { text: string; matched: Uint8Array } {
  let total = 0
  let firstMatch = -1
  for (const p of pieces) {
    if (p.match && p.text !== '' && firstMatch < 0) firstMatch = total
    total += p.text.length
  }
  let lo = 0
  let hi = total
  if (total > limit) {
    lo = firstMatch < 0 ? 0 : Math.max(0, firstMatch - Math.min(BEFORE, limit >> 2))
    hi = Math.min(total, lo + limit)
  }
  const text: string[] = []
  const matched = new Uint8Array(hi - lo)
  let at = 0
  for (const p of pieces) {
    const from = Math.max(lo, at)
    const to = Math.min(hi, at + p.text.length)
    if (from < to) {
      text.push(p.text.slice(from - at, to - at))
      if (p.match) matched.fill(1, from - lo, to - lo)
    }
    at += p.text.length
    if (at >= hi) break
  }
  let joined = text.join('')
  let flags = matched
  // A window must not begin or end inside a surrogate pair.
  let start = 0
  let end = joined.length
  if (lo > 0 && isLow(joined.charCodeAt(0))) start = 1
  if (hi < total && end > start && isHigh(joined.charCodeAt(end - 1))) end--
  if (start > 0 || end < joined.length) {
    joined = joined.slice(start, end)
    flags = flags.slice(start, end)
  }
  if (lo === 0 && hi === total) return { text: joined, matched: flags }
  // Say it is cut, except where the server's own cut already says so.
  const before = lo > 0 && !joined.startsWith(ELLIPSIS)
  const after = hi < total && !joined.endsWith(ELLIPSIS)
  if (!before && !after) return { text: joined, matched: flags }
  const lead = before ? 1 : 0
  const out = new Uint8Array(flags.length + lead + (after ? 1 : 0))
  out.set(flags, lead)
  return { text: (before ? ELLIPSIS : '') + joined + (after ? ELLIPSIS : ''), matched: out }
}

const isHigh = (c: number) => c >= 0xd800 && c <= 0xdbff
const isLow = (c: number) => c >= 0xdc00 && c <= 0xdfff

const isSpace = (c: string) => c === '' || /\s/.test(c)
const isWord = (c: string) => c !== '' && /[\p{L}\p{N}]/u.test(c)

// The text of a Markdown document's snippet, without its marks.
function clean(source: string, sourceMatched: Uint8Array): SnippetPiece[] {
  // 1. Blocks: fences, quotes, headings and bullets, line by line. What is left is one
  //    paragraph of running text (lines joined by a space), with the characters of a code block
  //    marked as such (lit): they are text, whatever they look like.
  const t: string[] = []
  const matchedChars: number[] = []
  const lit: number[] = []
  const put = (s: string, from: number, to: number, isLit: number) => {
    for (let i = from; i < to; i++) {
      t.push(s[i])
      matchedChars.push(sourceMatched[i])
      lit.push(isLit)
    }
  }
  const anyMatch = (from: number, to: number) => {
    for (let i = from; i < to; i++) if (sourceMatched[i]) return true
    return false
  }

  let fence = '' // the marker of the open code block: ``` or ~~~, with its length
  let at = 0
  while (at <= source.length) {
    let end = source.indexOf('\n', at)
    if (end < 0) end = source.length
    const fenceMark = fenceOf(source, at, end)
    if (
      fenceMark &&
      (fence === '' || (fenceMark[0] === fence[0] && fenceMark.length >= fence.length && closing(source, at, end)))
    ) {
      // A fence line is not text, unless a match is on it.
      if (anyMatch(at, end)) put(source, at, end, 1)
      fence = fence === '' ? fenceMark : ''
    } else if (fence !== '') {
      put(source, at, end, 1)
    } else {
      const content = afterBlockMarks(source, at, end)
      put(source, anyMatch(at, content) ? at : content, end, 0)
    }
    if (end < source.length) {
      t.push(' ')
      matchedChars.push(0)
      lit.push(0)
    }
    at = end + 1
  }

  // 2. Inline marks, over the whole paragraph.
  const n = t.length
  const m = Uint8Array.from(matchedChars)
  const keep = new Uint8Array(n).fill(1)
  const insertBefore = new Map<number, string>()
  const isLit = (i: number) => lit[i] === 1
  // matches[i]: how many matched characters are before i.
  const matches = new Int32Array(n + 1)
  for (let i = 0; i < n; i++) matches[i + 1] = matches[i] + m[i]
  const hasMatch = (from: number, to: number) => matches[to] - matches[from] > 0

  // nextOf[c][i]: the first position at or after i with that character, outside code; n if none.
  const nextOf = (c: string) => {
    const next = new Int32Array(n + 2)
    next[n] = n
    next[n + 1] = n
    for (let i = n - 1; i >= 0; i--) next[i] = t[i] === c && !isLit(i) ? i : next[i + 1]
    return next
  }
  const nextBracket = nextOf(']')
  const nextParen = nextOf(')')

  // The runs of backticks outside code blocks, by length, for finding where a code span ends.
  const ticks = new Map<number, { starts: number[]; next: number }>()
  for (let i = 0; i < n; ) {
    if (t[i] === '`' && !isLit(i)) {
      let j = i
      while (j < n && t[j] === '`' && !isLit(j)) j++
      const list = ticks.get(j - i)
      if (list) list.starts.push(i)
      else ticks.set(j - i, { starts: [i], next: 0 })
      i = j
    } else i++
  }

  type Run = { ch: string; lo: number; hi: number }
  const stacks: Record<string, Run[]> = { '*': [], _: [], '~': [] }
  const strays: Run[] = []

  const dropOne = (i: number) => {
    if (!m[i]) keep[i] = 0
  }
  // Drops marks that come in a pair only when neither holds a match, so no mark is left whose
  // partner went.
  const dropPair = (a: number, aTo: number, b: number, bTo: number) => {
    if (hasMatch(a, aTo) || hasMatch(b, bTo)) return
    keep.fill(0, a, aTo)
    keep.fill(0, b, bTo)
  }

  let linkClose = -1 // while in a link's words: where they end, and the destination begins
  let linkEnd = -1 // and where the destination ends (its ")")
  for (let i = 0; i < n; ) {
    if (i === linkClose) {
      i = linkEnd + 1
      linkClose = -1
      continue
    }
    const c = t[i]
    if (isLit(i)) {
      i++
      continue
    }
    if (c === '`') {
      let j = i
      while (j < n && t[j] === '`' && !isLit(j)) j++
      const len = j - i
      const list = ticks.get(len)
      let close = -1
      if (list) {
        // Runs are in order and the scan never goes back, so each is passed once.
        while (list.next < list.starts.length && list.starts[list.next] < j) list.next++
        if (list.next < list.starts.length) close = list.starts[list.next]
      }
      if (close < 0) {
        i = j // no end: the backticks are text
        continue
      }
      dropPair(i, j, close, close + len)
      // What is between them is code: text, whatever it looks like.
      for (let k = j; k < close; k++) lit[k] = 1
      i = close + len
      continue
    }
    if (c === '[' && linkClose < 0) {
      const close = nextBracket[i + 1]
      if (close < n && t[close + 1] === '(' && close + 1 < n) {
        const paren = nextParen[close + 2]
        if (paren < n) {
          const image = i > 0 && t[i - 1] === '!' && !isLit(i - 1) && keep[i - 1] === 1
          const brackets = !m[i] && !m[close]
          if (brackets) {
            if (!hasMatch(close + 1, paren + 1)) {
              keep[i] = 0
              keep.fill(0, close, paren + 1)
              if (image && !m[i - 1]) keep[i - 1] = 0
            } else {
              // The match is in the destination: it stays, after the link's words.
              keep[i] = 0
              keep[close] = 0
              insertBefore.set(close + 1, ' ')
              if (image && !m[i - 1]) keep[i - 1] = 0
            }
            linkClose = close
            linkEnd = paren
          }
        }
      }
      i++
      continue
    }
    if (c === '*' || c === '_' || c === '~') {
      let j = i
      while (j < n && t[j] === c && !isLit(j)) j++
      const len = j - i
      if (c === '~' && len < 2) {
        i = j
        continue
      }
      const prev = i > 0 ? t[i - 1] : ''
      const next = j < n ? t[j] : ''
      let canOpen: boolean
      let canClose: boolean
      if (c === '~') {
        canOpen = !isSpace(next)
        canClose = !isSpace(prev)
      } else if (isWord(prev) && isWord(next)) {
        canOpen = false
        canClose = false // inside a word: text
      } else {
        canOpen = !isSpace(next)
        canClose = !isSpace(prev)
      }
      const run: Run = { ch: c, lo: i, hi: j }
      if (canClose) {
        const stack = stacks[c]
        while (run.hi > run.lo && stack.length > 0) {
          const open = stack[stack.length - 1]
          const strong = run.hi - run.lo >= 2 && open.hi - open.lo >= 2
          const k = strong ? 2 : c === '~' ? 0 : 1
          if (k === 0) break
          dropPair(open.hi - k, open.hi, run.lo, run.lo + k)
          open.hi -= k
          run.lo += k
          if (open.hi === open.lo) stack.pop()
        }
      }
      if (run.hi > run.lo && canOpen) stacks[c].push(run)
      if (c === '*' && (canOpen || canClose)) strays.push(run)
      i = j
      continue
    }
    i++
  }
  // A "**" with no partner (the snippet was cut in the middle of a bold run) is left out too.
  for (const run of strays) if (run.hi - run.lo >= 2) for (let i = run.lo; i < run.hi; i++) dropOne(i)

  // 3. Lines run together, and the ends are trimmed. A snippet is a line of running text.
  // A blank that isn't matched is a gap; a gap is one space, and none at either end.
  const outKeep = new Uint8Array(n)
  const spaces = new Map<number, string>()
  let lastKept = -1
  for (let i = n - 1; i >= 0 && lastKept < 0; i--) if (keep[i] && (m[i] || !/\s/.test(t[i]))) lastKept = i
  let gap = false
  let started = false
  for (let i = 0; i <= lastKept; i++) {
    if (!keep[i]) continue
    if (!m[i] && /\s/.test(t[i])) {
      gap = true
      continue
    }
    if (started && (gap || insertBefore.has(i))) spaces.set(i, ' ')
    gap = false
    started = true
    outKeep[i] = 1
  }
  return group(t.join(''), m, outKeep, spaces)
}

// The kept characters as pieces: runs of matched and of other text, each with its spaces.
function group(text: string, matched: Uint8Array, kept: Uint8Array, spaces: Map<number, string>): SnippetPiece[] {
  const out: SnippetPiece[] = []
  let current = ''
  let match = false
  const flush = () => {
    if (current !== '') out.push(match ? { text: current, match: true } : { text: current })
    current = ''
  }
  for (let i = 0; i < text.length; i++) {
    if (!kept[i]) continue
    const space = spaces.get(i)
    if (space !== undefined) {
      if (match) flush()
      match = false
      current += space
    }
    const isMatch = matched[i] === 1
    if (isMatch !== match) {
      flush()
      match = isMatch
    }
    current += text[i]
  }
  flush()
  return out
}

// A code fence's marker (``` or ~~~, of any length from three) if the line begins with one.
function fenceOf(s: string, from: number, to: number): string {
  let i = from
  while (i < to && (s[i] === ' ' || s[i] === '\t')) i++
  const c = s[i]
  if (c !== '`' && c !== '~') return ''
  let j = i
  while (j < to && s[j] === c) j++
  return j - i >= 3 ? s.slice(i, j) : ''
}

// Whether the fence on this line is only the marker: what ends a block (an opening one may have
// a language after it).
function closing(s: string, from: number, to: number): boolean {
  let i = from
  while (i < to && (s[i] === ' ' || s[i] === '\t')) i++
  const c = s[i]
  while (i < to && s[i] === c) i++
  while (i < to && (s[i] === ' ' || s[i] === '\t' || s[i] === '\r')) i++
  return i === to
}

// Where a line's text begins after its block marks: quote markers, then a heading's #s or a
// bullet's marker and task box.
function afterBlockMarks(s: string, from: number, to: number): number {
  let i = from
  for (;;) {
    let j = i
    while (j < to && (s[j] === ' ' || s[j] === '\t')) j++
    if (s[j] !== '>') break
    j++
    if (s[j] === ' ' || s[j] === '\t') j++
    i = j
  }
  // A heading: up to three spaces, one to six #, then blanks.
  let j = i
  while (j < to && j - i < 3 && s[j] === ' ') j++
  const hashes = j
  while (j < to && s[j] === '#') j++
  if (j > hashes && j - hashes <= 6 && (s[j] === ' ' || s[j] === '\t')) {
    while (j < to && (s[j] === ' ' || s[j] === '\t')) j++
    return j
  }
  // A bullet, with its task box if it has one.
  j = i
  while (j < to && (s[j] === ' ' || s[j] === '\t')) j++
  if ((s[j] === '-' || s[j] === '*' || s[j] === '+') && (s[j + 1] === ' ' || s[j + 1] === '\t')) {
    j++
    while (j < to && (s[j] === ' ' || s[j] === '\t')) j++
    if (s[j] === '[' && /[ xX]/.test(s[j + 1] ?? '') && s[j + 2] === ']' && (s[j + 3] === ' ' || s[j + 3] === '\t')) {
      j += 3
      while (j < to && (s[j] === ' ' || s[j] === '\t')) j++
    }
    return j
  }
  return i
}
