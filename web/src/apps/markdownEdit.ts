export interface Edit {
  text: string
  start: number
  end: number
}

const LETTER_OR_DIGIT = /[\p{L}\p{N}]/u

const wrapped = (text: string, marker: string) =>
  text.length >= marker.length * 2 &&
  text.startsWith(marker) &&
  text.endsWith(marker) &&
  (marker.length > 1 || (text[1] !== marker && text[text.length - 2] !== marker))

const touchesWord = (text: string, from: number, to: number) =>
  LETTER_OR_DIGIT.test(text[from - 1] ?? '') || LETTER_OR_DIGIT.test(text[to] ?? '')

function wrapLines(text: string, from: number, to: number, marker: string, inword: string): Edit {
  const lines = text.slice(from, to).split('\n')
  const bodies = lines.map((line) => line.trim())
  const off = [marker, inword].find((m) => bodies.every((body) => body === '' || wrapped(body, m)))
  const on = touchesWord(text, from, to) ? inword : marker
  const body = lines
    .map((line, i) => {
      const inner = bodies[i]
      if (inner === '') return line
      const at = line.indexOf(inner)
      const next = off
        ? inner.slice(off.length, inner.length - off.length)
        : wrapped(inner, on)
          ? inner
          : on + inner + on
      return line.slice(0, at) + next + line.slice(at + inner.length)
    })
    .join('\n')
  return { text: text.slice(0, from) + body + text.slice(to), start: from, end: from + body.length }
}

export function wrap(
  text: string,
  start: number,
  end: number,
  marker: string,
  placeholder: string,
  inword = marker,
): Edit {
  const raw = text.slice(start, end)
  const from = start + (raw.length - raw.trimStart().length)
  const to = raw.trim() === '' ? from : end - (raw.length - raw.trimEnd().length)
  const selected = text.slice(from, to)
  if (selected.includes('\n')) return wrapLines(text, from, to, marker, inword)
  for (const m of new Set([marker, inword])) {
    const size = m.length
    const around =
      from >= size &&
      text.slice(from - size, from) === m &&
      text.slice(to, to + size) === m &&
      (size > 1 || (text[from - 2] !== m && text[to + 1] !== m))
    if (around) {
      return {
        text: text.slice(0, from - size) + selected + text.slice(to + size),
        start: from - size,
        end: to - size,
      }
    }
    if (wrapped(selected, m)) {
      const inner = selected.slice(size, selected.length - size)
      return { text: text.slice(0, from) + inner + text.slice(to), start: from, end: from + inner.length }
    }
  }
  const m = touchesWord(text, from, to) ? inword : marker
  const body = selected === '' ? placeholder : selected
  return {
    text: text.slice(0, from) + m + body + m + text.slice(to),
    start: from + m.length,
    end: from + m.length + body.length,
  }
}

export const normalizeBreaks = (text: string) => text.replace(/\r\n?/g, '\n')

export function splice(before: string, after: string) {
  const most = Math.min(before.length, after.length)
  let head = 0
  while (head < most && before[head] === after[head]) head++
  let tail = 0
  while (tail < most - head && before[before.length - 1 - tail] === after[after.length - 1 - tail]) tail++
  return { at: head, removed: before.length - head - tail, insert: after.slice(head, after.length - tail) }
}

export function keepBreaks(previous: string, next: string): string {
  if (!previous.includes('\r')) return next
  const was = normalizeBreaks(previous)
  if (was === next) return previous
  const starts: number[] = []
  for (let i = 0; i < previous.length; i++) {
    starts.push(i)
    if (previous[i] === '\r' && previous[i + 1] === '\n') i++
  }
  starts.push(previous.length)
  const { at, removed, insert } = splice(was, next)
  const replaced = previous.slice(starts[at], starts[at + removed])
  const separators = replaced.match(/\r\n?|\n/g) ?? []
  let used = 0
  const kept = insert.replace(/\n/g, () => separators[used++] ?? '\n')
  return previous.slice(0, starts[at]) + kept + previous.slice(starts[at + removed])
}

export const HEADING_PREFIX = '### '
export const LIST_PREFIX = '- '
export const CHECKLIST_PREFIX = '- [ ] '

const HEADING_FAMILY = /^#{1,6} /
const LIST_FAMILY = /^- (\[[ xX]\] )?/

export function prefixLines(text: string, start: number, end: number, prefix: string): Edit {
  const family = prefix === HEADING_PREFIX ? HEADING_FAMILY : LIST_FAMILY
  const from = start === 0 ? 0 : text.lastIndexOf('\n', start - 1) + 1
  const stop = text.indexOf('\n', end > start && text[end - 1] === '\n' ? end - 1 : end)
  const to = stop === -1 ? text.length : stop
  const lines = text.slice(from, to).split('\n')
  const filled = lines.filter((line) => line.trim() !== '')
  const targets = filled.length === 0 ? lines : filled
  const marker = (line: string) => family.exec(line)?.[0] ?? ''
  const allHave = targets.every((line) => marker(line).replace(/\[[xX]\]/, '[ ]') === prefix)
  const steps = lines.map((line) => {
    if (line.trim() === '' && filled.length > 0) return { line, removed: 0, added: 0 }
    const removed = marker(line).length
    return { line: (allHave ? '' : prefix) + line.slice(removed), removed, added: allHave ? 0 : prefix.length }
  })
  const body = steps.map((step) => step.line).join('\n')
  const place = (pos: number) => {
    if (pos < from) return pos
    if (pos > to) return pos + body.length - (to - from)
    let before = 0
    let after = 0
    for (const step of steps) {
      const length = step.line.length - step.added + step.removed
      if (pos <= from + before + length) {
        return from + after + step.added + Math.max(0, pos - from - before - step.removed)
      }
      before += length + 1
      after += step.line.length + 1
    }
    return to
  }
  return { text: text.slice(0, from) + body + text.slice(to), start: place(start), end: place(end) }
}

export function link(text: string, start: number, end: number, label: string, address: string): Edit {
  const selected = text.slice(start, end)
  const body = selected === '' ? label : selected
  const head = `[${body}](`
  const base = start + head.length
  return {
    text: `${text.slice(0, start)}${head}${address})${text.slice(end)}`,
    start: selected === '' ? start + 1 : base,
    end: selected === '' ? start + 1 + body.length : base + address.length,
  }
}
