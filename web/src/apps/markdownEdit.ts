export interface Edit {
  text: string
  start: number
  end: number
}

export function wrap(text: string, start: number, end: number, marker: string, placeholder: string): Edit {
  const size = marker.length
  const selected = text.slice(start, end)
  const around = text.slice(start - size, start) === marker && text.slice(end, end + size) === marker
  if (around && start >= size) {
    return {
      text: text.slice(0, start - size) + selected + text.slice(end + size),
      start: start - size,
      end: end - size,
    }
  }
  if (selected.length >= size * 2 && selected.startsWith(marker) && selected.endsWith(marker)) {
    const inner = selected.slice(size, selected.length - size)
    return { text: text.slice(0, start) + inner + text.slice(end), start, end: start + inner.length }
  }
  const body = selected === '' ? placeholder : selected
  return {
    text: text.slice(0, start) + marker + body + marker + text.slice(end),
    start: start + size,
    end: start + size + body.length,
  }
}

export const HEADING_PREFIX = '### '
export const LIST_PREFIX = '- '
export const CHECKLIST_PREFIX = '- [ ] '

const HEADING_FAMILY = /^#{1,6} /
const LIST_FAMILY = /^- (\[[ xX]\] )?/

export function prefixLines(text: string, start: number, end: number, prefix: string): Edit {
  const family = prefix === HEADING_PREFIX ? HEADING_FAMILY : LIST_FAMILY
  const from = text.lastIndexOf('\n', start - 1) + 1
  const stop = text.indexOf('\n', end > start && text[end - 1] === '\n' ? end - 1 : end)
  const to = stop === -1 ? text.length : stop
  const lines = text.slice(from, to).split('\n')
  const filled = lines.filter((line) => line.trim() !== '')
  const targets = filled.length === 0 ? lines : filled
  const marker = (line: string) => family.exec(line)?.[0] ?? ''
  const allHave = targets.every((line) => marker(line).replace(/\[[xX]\]/, '[ ]') === prefix)
  const changed = lines.map((line) => {
    if (line.trim() === '' && filled.length > 0) return line
    if (allHave) return line.slice(marker(line).length)
    return prefix + line.slice(marker(line).length)
  })
  const body = changed.join('\n')
  const grew = body.length - (to - from)
  const first = changed[0].length - lines[0].length
  return {
    text: text.slice(0, from) + body + text.slice(to),
    start: Math.max(from, start + first),
    end: end + grew,
  }
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
