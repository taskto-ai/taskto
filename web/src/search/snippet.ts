export type SnippetPiece = { text: string; match?: boolean }

export type SnippetOptions = {
  markdown?: boolean
  limit?: number
}

export const isMarkdownPath = (path: string) => /\.md$/i.test(path)

export const LIMIT = 1200
const BEFORE = 300

const ELLIPSIS = '…'

export function cleanSnippet(pieces: SnippetPiece[], options: SnippetOptions = {}): SnippetPiece[] {
  const { text, matched } = bound(pieces, options.limit ?? LIMIT)
  if (options.markdown === false) return group(text, matched, new Uint8Array(text.length).fill(1), new Map())
  return clean(text, matched)
}

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
  let start = 0
  let end = joined.length
  if (lo > 0 && isLow(joined.charCodeAt(0))) start = 1
  if (hi < total && end > start && isHigh(joined.charCodeAt(end - 1))) end--
  if (start > 0 || end < joined.length) {
    joined = joined.slice(start, end)
    flags = flags.slice(start, end)
  }
  if (lo === 0 && hi === total) return { text: joined, matched: flags }
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

const PUNCTUATION = /^[!-/:-@[-`{-~]$/
const isSpace = (c: string) => c === '' || /\s/.test(c)
const isWord = (c: string) => c !== '' && /[\p{L}\p{N}]/u.test(c)

function clean(source: string, sourceMatched: Uint8Array): SnippetPiece[] {
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

  let fence = ''
  let at = 0
  while (at <= source.length) {
    let end = source.indexOf('\n', at)
    if (end < 0) end = source.length
    const fenceMark = fenceOf(source, at, end)
    if (
      fenceMark &&
      (fence === '' || (fenceMark[0] === fence[0] && fenceMark.length >= fence.length && closing(source, at, end)))
    ) {
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

  const n = t.length
  const m = Uint8Array.from(matchedChars)
  const keep = new Uint8Array(n).fill(1)
  const insertBefore = new Map<number, string>()
  const escaped = new Uint8Array(n)
  for (let i = 0; i + 1 < n; ) {
    if (t[i] === '\\' && lit[i] === 0 && PUNCTUATION.test(t[i + 1])) {
      escaped[i + 1] = 1
      i += 2
    } else i++
  }
  const isLit = (i: number) => lit[i] === 1 || escaped[i] === 1
  const matches = new Int32Array(n + 1)
  for (let i = 0; i < n; i++) matches[i + 1] = matches[i] + m[i]
  const hasMatch = (from: number, to: number) => matches[to] - matches[from] > 0

  const nextOf = (c: string) => {
    const next = new Int32Array(n + 2)
    next[n] = n
    next[n + 1] = n
    for (let i = n - 1; i >= 0; i--) next[i] = t[i] === c && !isLit(i) ? i : next[i + 1]
    return next
  }
  const nextBracket = nextOf(']')
  const closeOf = new Int32Array(n).fill(-1)
  const opened: number[] = []
  for (let i = 0; i < n; i++) {
    if (isLit(i)) continue
    if (t[i] === '(') opened.push(i)
    else if (t[i] === ')' && opened.length > 0) closeOf[opened.pop() as number] = i
  }

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
  const dropPair = (a: number, aTo: number, b: number, bTo: number) => {
    if (hasMatch(a, aTo) || hasMatch(b, bTo)) return
    keep.fill(0, a, aTo)
    keep.fill(0, b, bTo)
  }

  let linkClose = -1
  let linkEnd = -1
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
    if (c === '\\' && i + 1 < n && escaped[i + 1] === 1) {
      dropOne(i)
      i += 2
      continue
    }
    if (c === '`') {
      let j = i
      while (j < n && t[j] === '`' && !isLit(j)) j++
      const len = j - i
      const list = ticks.get(len)
      let close = -1
      if (list) {
        while (list.next < list.starts.length && list.starts[list.next] < j) list.next++
        if (list.next < list.starts.length) close = list.starts[list.next]
      }
      if (close < 0) {
        i = j
        continue
      }
      dropPair(i, j, close, close + len)
      for (let k = j; k < close; k++) lit[k] = 1
      i = close + len
      continue
    }
    if (c === '[' && linkClose < 0) {
      const close = nextBracket[i + 1]
      if (close < n && t[close + 1] === '(' && close + 1 < n) {
        const paren = isLit(close + 1) ? -1 : closeOf[close + 1]
        if (paren > 0) {
          const image = i > 0 && t[i - 1] === '!' && !isLit(i - 1) && keep[i - 1] === 1
          const brackets = !m[i] && !m[close]
          if (brackets) {
            if (!hasMatch(close + 1, paren + 1)) {
              keep[i] = 0
              keep.fill(0, close, paren + 1)
              if (image && !m[i - 1]) keep[i - 1] = 0
            } else {
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
        canClose = false
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
  for (const run of strays) if (run.hi - run.lo >= 2) for (let i = run.lo; i < run.hi; i++) dropOne(i)

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

function fenceOf(s: string, from: number, to: number): string {
  let i = from
  while (i < to && (s[i] === ' ' || s[i] === '\t')) i++
  const c = s[i]
  if (c !== '`' && c !== '~') return ''
  let j = i
  while (j < to && s[j] === c) j++
  return j - i >= 3 ? s.slice(i, j) : ''
}

function closing(s: string, from: number, to: number): boolean {
  let i = from
  while (i < to && (s[i] === ' ' || s[i] === '\t')) i++
  const c = s[i]
  while (i < to && s[i] === c) i++
  while (i < to && (s[i] === ' ' || s[i] === '\t' || s[i] === '\r')) i++
  return i === to
}

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
  let j = i
  while (j < to && j - i < 3 && s[j] === ' ') j++
  const hashes = j
  while (j < to && s[j] === '#') j++
  if (j > hashes && j - hashes <= 6 && (s[j] === ' ' || s[j] === '\t')) {
    while (j < to && (s[j] === ' ' || s[j] === '\t')) j++
    return j
  }
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
