export const MAX_LINES = 20000
export const MAX_EDITS = 5000
const MAX_WORD_TOKENS = 4000
const MAX_WORD_EDITS = 1000
const CONTEXT = 3
const NO_EOL = '\u0000'

export type LineKind = 'same' | 'add' | 'del'

export type Line = {
  kind: LineKind
  text: string
  aLine?: number
  bLine?: number
  noEol: boolean
}

export type Hunk = {
  aStart: number
  bStart: number
  aCount: number
  bCount: number
  start: number
  end: number
  lines: Line[]
}

export type LineDiff =
  | { tooLarge: true }
  | { tooLarge: false; added: number; removed: number; lines: Line[]; hunks: Hunk[] }

export type WordPart = { text: string; changed: boolean }

type Op = { kind: LineKind; a: number; b: number }

export function splitLines(text: string): string[] {
  const normal = text.replace(/\r\n/g, '\n')
  if (normal === '') return []
  const parts = normal.split('\n')
  if (normal.endsWith('\n')) {
    parts.pop()
    return parts
  }
  parts[parts.length - 1] += NO_EOL
  return parts
}

const textOf = (key: string) => (key.endsWith(NO_EOL) ? key.slice(0, -1) : key)

function myers(a: string[], b: string[], limit: number): Op[] | null {
  const n = a.length
  const m = b.length
  const max = Math.min(n + m, limit)
  const offset = max + 1
  const v = new Int32Array(2 * max + 3)
  const trace: Int32Array[] = []
  for (let d = 0; d <= max; d++) {
    for (let k = -d; k <= d; k += 2) {
      let x: number
      if (k === -d || (k !== d && v[offset + k - 1] < v[offset + k + 1])) x = v[offset + k + 1]
      else x = v[offset + k - 1] + 1
      let y = x - k
      while (x < n && y < m && a[x] === b[y]) {
        x++
        y++
      }
      v[offset + k] = x
      if (x >= n && y >= m) return backtrack(a, b, trace, d)
    }
    const snapshot = new Int32Array(d + 1)
    for (let k = -d; k <= d; k += 2) snapshot[(k + d) / 2] = v[offset + k]
    trace.push(snapshot)
  }
  return null
}

function backtrack(a: string[], b: string[], trace: Int32Array[], edits: number): Op[] {
  const out: Op[] = []
  let x = a.length
  let y = b.length
  for (let d = edits; d > 0; d--) {
    const k = x - y
    const prev = trace[d - 1]
    const at = (diagonal: number) => prev[(diagonal + d - 1) / 2]
    const down = k === -d || (k !== d && at(k - 1) < at(k + 1))
    const prevK = down ? k + 1 : k - 1
    const prevX = at(prevK)
    const prevY = prevX - prevK
    const snakeX = down ? prevX : prevX + 1
    const snakeY = down ? prevY + 1 : prevY
    while (x > snakeX && y > snakeY) {
      x--
      y--
      out.push({ kind: 'same', a: x, b: y })
    }
    if (down) out.push({ kind: 'add', a: -1, b: prevY })
    else out.push({ kind: 'del', a: prevX, b: -1 })
    x = prevX
    y = prevY
  }
  while (x > 0 && y > 0) {
    x--
    y--
    out.push({ kind: 'same', a: x, b: y })
  }
  return out.reverse()
}

function diffKeys(a: string[], b: string[], limit: number): Op[] | null {
  let head = 0
  while (head < a.length && head < b.length && a[head] === b[head]) head++
  let tail = 0
  while (tail < a.length - head && tail < b.length - head && a[a.length - 1 - tail] === b[b.length - 1 - tail]) tail++
  const middle = myers(a.slice(head, a.length - tail), b.slice(head, b.length - tail), limit)
  if (!middle) return null
  const ops: Op[] = []
  for (let i = 0; i < head; i++) ops.push({ kind: 'same', a: i, b: i })
  for (const op of middle) ops.push({ kind: op.kind, a: op.a < 0 ? -1 : op.a + head, b: op.b < 0 ? -1 : op.b + head })
  for (let i = tail; i > 0; i--) ops.push({ kind: 'same', a: a.length - i, b: b.length - i })
  return groupChanges(ops)
}

function groupChanges(ops: Op[]): Op[] {
  const out: Op[] = []
  let dels: Op[] = []
  let adds: Op[] = []
  const flush = () => {
    out.push(...dels, ...adds)
    dels = []
    adds = []
  }
  for (const op of ops) {
    if (op.kind === 'del') dels.push(op)
    else if (op.kind === 'add') adds.push(op)
    else {
      flush()
      out.push(op)
    }
  }
  flush()
  return out
}

export function diffLines(oldText: string, newText: string): LineDiff {
  const a = splitLines(oldText)
  const b = splitLines(newText)
  if (a.length > MAX_LINES || b.length > MAX_LINES) return { tooLarge: true }
  const ops = diffKeys(a, b, MAX_EDITS)
  if (!ops) return { tooLarge: true }
  let added = 0
  let removed = 0
  const lines: Line[] = ops.map((op) => {
    if (op.kind === 'add') added++
    if (op.kind === 'del') removed++
    const key = op.kind === 'add' ? b[op.b] : a[op.a]
    return {
      kind: op.kind,
      text: textOf(key),
      aLine: op.kind === 'add' ? undefined : op.a + 1,
      bLine: op.kind === 'del' ? undefined : op.b + 1,
      noEol: key.endsWith(NO_EOL),
    }
  })
  return { tooLarge: false, added, removed, lines, hunks: hunksOf(lines) }
}

function hunksOf(lines: Line[]): Hunk[] {
  const keep = new Uint8Array(lines.length)
  lines.forEach((line, i) => {
    if (line.kind === 'same') return
    for (let j = Math.max(0, i - CONTEXT); j <= Math.min(lines.length - 1, i + CONTEXT); j++) keep[j] = 1
  })
  const hunks: Hunk[] = []
  let aNext = 1
  let bNext = 1
  let open: Hunk | null = null
  lines.forEach((line, i) => {
    if (keep[i]) {
      if (!open) {
        open = { aStart: aNext, bStart: bNext, aCount: 0, bCount: 0, start: i, end: i, lines: [] }
        hunks.push(open)
      }
      open.lines.push(line)
      open.end = i + 1
      if (line.kind !== 'add') open.aCount++
      if (line.kind !== 'del') open.bCount++
    } else open = null
    if (line.kind !== 'add') aNext++
    if (line.kind !== 'del') bNext++
  })
  return hunks
}

const WORD_FALLBACK = /\s+|[\p{L}\p{N}_]+|[^\s\p{L}\p{N}_]/gu

export function tokenize(text: string): string[] {
  if (typeof Intl !== 'undefined' && 'Segmenter' in Intl) {
    const segmenter = new Intl.Segmenter(undefined, { granularity: 'word' })
    return Array.from(segmenter.segment(text), (s) => s.segment)
  }
  return text.match(WORD_FALLBACK) ?? []
}

function mergeParts(parts: WordPart[]): WordPart[] {
  const out: WordPart[] = []
  for (const part of parts) {
    const last = out.at(-1)
    if (last && last.changed === part.changed) last.text += part.text
    else out.push({ ...part })
  }
  return out
}

export function diffWords(oldText: string, newText: string): { old: WordPart[]; new: WordPart[] } | null {
  const a = tokenize(oldText)
  const b = tokenize(newText)
  if (a.length > MAX_WORD_TOKENS || b.length > MAX_WORD_TOKENS) return null
  const ops = diffKeys(a, b, MAX_WORD_EDITS)
  if (!ops) return null
  const old: WordPart[] = []
  const added: WordPart[] = []
  for (const op of ops) {
    if (op.kind === 'same') {
      old.push({ text: a[op.a], changed: false })
      added.push({ text: b[op.b], changed: false })
    } else if (op.kind === 'del') old.push({ text: a[op.a], changed: true })
    else added.push({ text: b[op.b], changed: true })
  }
  return { old: mergeParts(old), new: mergeParts(added) }
}
