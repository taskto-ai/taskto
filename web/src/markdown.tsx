// A small Markdown renderer for previews: headings, paragraphs, lists, quotes, code, rules,
// emphasis and links, built as React elements. Nothing is ever inserted as HTML, so raw HTML
// in a document shows as its text; links keep only http(s), mailto and relative targets, and
// images show as links rather than loading from anywhere. It covers the common syntax, not
// all of CommonMark. Shared by every page that shows Markdown (documents, cards, comments).

import { createContext, Fragment, type MouseEvent, type ReactNode, useContext } from 'react'

// A link target that is safe to follow, or null. Browsers drop tabs, newlines and other
// control characters inside a URL's scheme ("java\tscript:"), so those are removed before
// looking at it, and anything with a scheme other than http, https or mailto is refused.
export function safeHref(raw: string): string | null {
  const href = raw.trim()
  // biome-ignore lint/suspicious/noControlCharactersInRegex: removing them is the point
  const bare = href.replace(/[\u0000- \u007f-\u009f]/g, '')
  if (bare === '') return null
  const scheme = /^([a-z][a-z0-9+.-]*):/i.exec(bare)
  if (scheme && !/^(https?|mailto)$/i.test(scheme[1])) return null
  // And the browser's own URL parser, the one that will follow the link, must agree: a
  // relative link or "//host/…" resolves against this page, so it is http(s) too.
  try {
    if (!['http:', 'https:', 'mailto:'].includes(new URL(href, 'https://taskto.invalid/').protocol)) return null
  } catch {
    return null
  }
  return href
}

// Browsers read a backslash in a URL as a slash, so "\\host" is another site too.
const external = (href: string) => /^(https?:)?[\\/]{2}/i.test(href) || /^mailto:/i.test(href)

// Nesting deeper than this (quotes in quotes, emphasis in links…) stays text, so a hostile
// document can't exhaust the stack.
const MAX_DEPTH = 16

type Block =
  | { kind: 'heading'; level: number; text: string }
  | { kind: 'paragraph'; text: string }
  | { kind: 'code'; text: string; lang?: string }
  | { kind: 'quote'; blocks: Block[] }
  | { kind: 'list'; ordered: boolean; start: number; items: Block[][] }
  | { kind: 'rule' }
  | { kind: 'table'; align: Align[]; head: string[]; rows: string[][] }

// A table column's alignment, from its rule (":--", "--:", ":-:").
type Align = 'left' | 'center' | 'right' | undefined

// A fence, and the language named after it ("```ts"), if any.
const FENCE = /^ {0,3}(`{3,}|~{3,})[ \t]*([^\s`]*)/
const HEADING = /^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$/
const RULE = /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/
const QUOTE = /^ {0,3}> ?/
const ITEM = /^( *)([-*+]|\d{1,9}[.)])([ \t]+|$)/
// The rule under a table's head row (GitHub's tables): cells of dashes, colons for alignment.
const TABLE_RULE = /^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$/
// The most rows and columns a table shows; the rest of a hostile one stays out.
const MAX_TABLE_ROWS = 1000
const MAX_TABLE_COLUMNS = 64

// A table row's cells, without its outer pipes; "\|" is a pipe inside a cell.
function splitRow(line: string): string[] {
  let row = line.trim()
  if (row.startsWith('|')) row = row.slice(1)
  if (row.endsWith('|') && !row.endsWith('\\|')) row = row.slice(0, -1)
  const cells: string[] = []
  let cell = ''
  for (let i = 0; i < row.length; i++) {
    if (row[i] === '\\' && row[i + 1] === '|') {
      cell += '|'
      i++
    } else if (row[i] === '|') {
      cells.push(cell.trim())
      cell = ''
    } else {
      cell += row[i]
    }
  }
  cells.push(cell.trim())
  return cells.slice(0, MAX_TABLE_COLUMNS)
}

// A table starts at line i: a row with a pipe, then a rule with as many cells.
const tableAt = (lines: string[], i: number) =>
  lines[i].includes('|') &&
  i + 1 < lines.length &&
  TABLE_RULE.test(lines[i + 1]) &&
  splitRow(lines[i + 1]).length === splitRow(lines[i]).length

const blank = (line: string) => line.trim() === ''
const startsBlock = (line: string) =>
  FENCE.test(line) || HEADING.test(line) || RULE.test(line) || QUOTE.test(line) || ITEM.test(line)

export function parseBlocks(text: string): Block[] {
  return blocks(text.replace(/\r\n?/g, '\n').split('\n'), 0)
}

function blocks(lines: string[], depth: number): Block[] {
  if (depth > MAX_DEPTH) return [{ kind: 'code', text: lines.join('\n') }]
  const out: Block[] = []
  let i = 0
  while (i < lines.length) {
    const line = lines[i]
    if (blank(line)) {
      i++
      continue
    }
    const fence = FENCE.exec(line)
    if (fence) {
      const close = new RegExp(`^ {0,3}${fence[1][0]}{${fence[1].length},}[ \\t]*$`)
      const body: string[] = []
      i++
      while (i < lines.length && !close.test(lines[i])) body.push(lines[i++])
      i++ // the closing fence, or past the end
      out.push({ kind: 'code', text: body.join('\n'), lang: fence[2] || undefined })
      continue
    }
    const heading = HEADING.exec(line)
    if (heading) {
      out.push({ kind: 'heading', level: heading[1].length, text: heading[2] ?? '' })
      i++
      continue
    }
    if (RULE.test(line)) {
      out.push({ kind: 'rule' })
      i++
      continue
    }
    if (QUOTE.test(line)) {
      const body: string[] = []
      while (i < lines.length && !blank(lines[i]) && (QUOTE.test(lines[i]) || !startsBlock(lines[i]))) {
        body.push(lines[i++].replace(QUOTE, ''))
      }
      out.push({ kind: 'quote', blocks: blocks(body, depth + 1) })
      continue
    }
    const item = ITEM.exec(line)
    if (item) {
      const indent = item[1].length
      const ordered = /\d/.test(item[2])
      const items: Block[][] = []
      while (i < lines.length) {
        // Blank lines between two items keep them in one list.
        let next = i
        while (next < lines.length && blank(lines[next])) next++
        if (next > i && next < lines.length && ITEM.exec(lines[next])?.[1].length === indent) i = next
        const m = ITEM.exec(lines[i])
        if (!m || m[1].length !== indent || /\d/.test(m[2]) !== ordered) break
        // The item's lines: its first, then those indented past the marker (a nested list,
        // more text), and lazy continuations of its paragraph.
        const width = m[0].length
        const body = [lines[i].slice(width)]
        i++
        while (i < lines.length) {
          const next = lines[i]
          const inner = ITEM.exec(next)
          if (blank(next)) {
            const after = lines[i + 1]
            if (after !== undefined && !blank(after) && after.length - after.trimStart().length > indent) {
              body.push('')
              i++
              continue
            }
            break
          }
          const nextIndent = next.length - next.trimStart().length
          if (nextIndent > indent) body.push(next.slice(Math.min(nextIndent, width)))
          else if (!inner && !startsBlock(next)) body.push(next)
          else break
          i++
        }
        items.push(blocks(body, depth + 1))
      }
      out.push({ kind: 'list', ordered, start: ordered ? Number.parseInt(item[2], 10) : 1, items })
      continue
    }
    if (tableAt(lines, i)) {
      const head = splitRow(line)
      const align = splitRow(lines[i + 1]).map((c): Align => {
        const left = c.startsWith(':')
        const right = c.endsWith(':')
        return left && right ? 'center' : right ? 'right' : left ? 'left' : undefined
      })
      const rows: string[][] = []
      i += 2
      while (i < lines.length && !blank(lines[i]) && lines[i].includes('|')) {
        if (rows.length < MAX_TABLE_ROWS) rows.push(splitRow(lines[i]))
        i++
      }
      out.push({ kind: 'table', align, head, rows })
      continue
    }
    const para: string[] = []
    while (
      i < lines.length &&
      !blank(lines[i]) &&
      (para.length === 0 || (!startsBlock(lines[i]) && !tableAt(lines, i)))
    ) {
      para.push(lines[i++])
    }
    out.push({ kind: 'paragraph', text: para.map((l) => l.trim()).join('\n') })
  }
  return out
}

// Inline text: code spans, escapes, links, images (as links), autolinks, strong, emphasis
// and strikethrough. Unmatched markers stay as text.
export function inline(text: string, onLink?: LinkHandler, depth = 0): ReactNode[] {
  if (depth > MAX_DEPTH) return [text]
  const out: ReactNode[] = []
  let plain = ''
  const flush = () => {
    if (plain) out.push(plain)
    plain = ''
  }
  const push = (node: ReactNode) => {
    flush()
    out.push(node)
  }
  let i = 0
  while (i < text.length) {
    const c = text[i]
    const rest = text.slice(i)
    if (c === '\\' && i + 1 < text.length && /[!-/:-@[-`{-~]/.test(text[i + 1])) {
      plain += text[i + 1]
      i += 2
      continue
    }
    if (c === '\\' && text[i + 1] === '\n') {
      push(<br key={out.length} />)
      i += 2
      continue
    }
    if (c === '`') {
      const run = /^`+/.exec(rest)?.[0] ?? '`'
      const end = text.indexOf(run, i + run.length)
      if (end > 0) {
        push(<code key={out.length}>{text.slice(i + run.length, end).replace(/\n/g, ' ')}</code>)
        i = end + run.length
        continue
      }
      plain += run
      i += run.length
      continue
    }
    if (c === '<') {
      const auto = /^<((?:https?:\/\/|mailto:)[^\s<>]+)>/i.exec(rest)
      if (auto) {
        push(<A key={out.length} href={auto[1]} onLink={onLink} label={auto[1]} />)
        i += auto[0].length
        continue
      }
    }
    // A bare http(s) URL, as models write them, starting a word. Trailing punctuation, and a
    // closing parenthesis it didn't open, are the sentence's, not the URL's.
    if ((c === 'h' || c === 'H') && !/[\p{L}\p{N}_]/u.test(text[i - 1] ?? '')) {
      const bare = /^https?:\/\/[^\s<>"'`]+/i.exec(text.slice(i, i + MAX_TARGET))
      if (bare) {
        let url = bare[0].replace(/[.,;:!?*_~]+$/, '')
        while (url.endsWith(')') && (url.match(/\(/g)?.length ?? 0) < (url.match(/\)/g)?.length ?? 0)) {
          url = url.slice(0, -1).replace(/[.,;:!?*_~]+$/, '')
        }
        if (/^https?:\/\/[^/?#]/i.test(url)) {
          push(<A key={out.length} href={url} onLink={onLink} label={url} />)
          i += url.length
          continue
        }
      }
    }
    if (c === '[' || (c === '!' && text[i + 1] === '[')) {
      const image = c === '!'
      const link = parseLink(text, image ? i + 1 : i)
      if (link) {
        const label = image ? [link.label || link.href] : inline(link.label, onLink, depth + 1)
        push(<A key={out.length} href={link.href} onLink={onLink} label={label} />)
        i = link.end
        continue
      }
    }
    const delim = /^(\*\*|__|~~|\*|_)/.exec(rest)?.[0]
    if (delim) {
      const end = closing(text, i, delim)
      if (end > 0) {
        const inner = inline(text.slice(i + delim.length, end), onLink, depth + 1)
        const key = out.length
        push(
          delim === '~~' ? (
            <del key={key}>{inner}</del>
          ) : delim.length === 2 ? (
            <strong key={key}>{inner}</strong>
          ) : (
            <em key={key}>{inner}</em>
          ),
        )
        i = end + delim.length
        continue
      }
      plain += delim
      i += delim.length
      continue
    }
    if (c === ' ' && /^ {2,}\n/.test(rest)) {
      push(<br key={out.length} />)
      i += rest.indexOf('\n') + 1
      continue
    }
    plain += c
    i++
  }
  flush()
  return out
}

// Where the run of `delim` opened at `from` closes, or -1. An underscore inside a word
// (snake_case) neither opens nor closes; the content can't start or end with a space.
// How far an emphasis may reach for its closer. Every unmatched "*" or "_" scans for one, so
// without a bound a paragraph of them costs a scan to its end each: quadratic, and a shared
// document could freeze the tab of whoever opens it (as unclosed "[" could, see MAX_LABEL).
const MAX_EMPHASIS = 1000

function closing(text: string, from: number, delim: string): number {
  const word = (ch: string | undefined) => ch !== undefined && /[\p{L}\p{N}]/u.test(ch)
  const open = from + delim.length
  if (delim[0] === '_' && word(text[from - 1])) return -1
  if (open >= text.length || /\s/.test(text[open])) return -1
  let j = open
  const stop = Math.min(text.length, open + MAX_EMPHASIS)
  while (j < stop) {
    if (text[j] === '\\') {
      j += 2
      continue
    }
    if (text[j] === '`') {
      const run = /^`+/.exec(text.slice(j, stop))?.[0] ?? '`'
      const end = text.indexOf(run, j + run.length)
      j = end > 0 && end < stop ? end + run.length : j + run.length
      continue
    }
    if (text.startsWith(delim, j) && j > open && !/\s/.test(text[j - 1])) {
      const after = text[j + delim.length]
      // A single * or _ next to another one is part of a double: not this closer.
      const doubled = delim.length === 1 && (after === delim || text[j - 1] === delim)
      if (!doubled && !(delim[0] === '_' && word(after))) return j
    }
    j++
  }
  return -1
}

// [label](href "title") starting at `at` (the "["), or null.
// A link's label and target are bounded (as in CommonMark), so a document full of "[" that
// never close costs a bounded scan at each one, not a scan to the end: no page can be made to
// freeze the tab of whoever opens it.
const MAX_LABEL = 999
const MAX_TARGET = 4096

function parseLink(text: string, at: number): { label: string; href: string; end: number } | null {
  let depth = 0
  let j = at
  const stop = Math.min(text.length, at + MAX_LABEL + 2)
  for (; j < stop; j++) {
    if (text[j] === '\\') j++
    else if (text[j] === '[') depth++
    else if (text[j] === ']' && --depth === 0) break
  }
  if (j >= stop || text[j + 1] !== '(') return null
  const m = /^\(\s*(<[^<>\n]*>|[^\s()]*(?:\([^\s()]*\)[^\s()]*)*)(?:\s+(?:"[^"]*"|'[^']*'))?\s*\)/.exec(
    text.slice(j + 1, j + 1 + MAX_TARGET),
  )
  if (!m) return null
  const href = m[1].startsWith('<') ? m[1].slice(1, -1) : m[1]
  return { label: text.slice(at + 1, j), href, end: j + 1 + m[0].length }
}

// A link the page handles itself (a document of the knowledge base, say): return what opens it,
// or null if the page has nowhere for it. A relative link the page doesn't handle shows as
// text: following it would leave the page, an editor included.
export type LinkHandler = (href: string) => (() => void) | null

function A({ href, label, onLink }: { href: string; label: ReactNode; onLink?: LinkHandler }) {
  const safe = safeHref(href)
  if (safe === null) return <span>{label}</span>
  if (external(safe)) {
    return (
      <a href={safe} target="_blank" rel="noopener noreferrer">
        {label}
      </a>
    )
  }
  const open = onLink?.(safe) ?? null
  if (!open) return <span>{label}</span>
  const click = (e: MouseEvent<HTMLAnchorElement>) => {
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
    e.preventDefault()
    open()
  }
  return (
    <a href={safe} onClick={click}>
      {label}
    </a>
  )
}

// Blocks have no identity but their place, so each is keyed by it.
function render(list: Block[], onLink?: LinkHandler, tight = false): ReactNode[] {
  // biome-ignore lint/suspicious/noArrayIndexKey: see above
  return list.map((b, k) => <Fragment key={k}>{block(b, onLink, tight)}</Fragment>)
}

function block(b: Block, onLink: LinkHandler | undefined, tight: boolean): ReactNode {
  switch (b.kind) {
    case 'heading':
      return <Heading level={b.level}>{inline(b.text, onLink)}</Heading>
    case 'paragraph':
      return <Paragraph text={b.text} onLink={onLink} tight={tight} />
    case 'code':
      return (
        <pre>
          <code data-lang={b.lang}>{b.text}</code>
        </pre>
      )
    case 'quote':
      return <blockquote>{render(b.blocks, onLink)}</blockquote>
    case 'rule':
      return <hr />
    case 'table': {
      // Each row has the head's cells: missing ones are empty, extra ones are dropped.
      const row = (cells: string[], Cell: 'th' | 'td') =>
        b.head.map((_, j) => (
          // biome-ignore lint/suspicious/noArrayIndexKey: cells have no identity but their place
          <Cell key={j} style={{ textAlign: b.align[j] }}>
            {inline(cells[j] ?? '', onLink)}
          </Cell>
        ))
      return (
        <div className="tt-md-table">
          <table>
            <thead>
              <tr>{row(b.head, 'th')}</tr>
            </thead>
            <tbody>
              {b.rows.map((r, n) => (
                // biome-ignore lint/suspicious/noArrayIndexKey: rows have no identity but their place
                <tr key={n}>{row(r, 'td')}</tr>
              ))}
            </tbody>
          </table>
        </div>
      )
    }
    case 'list': {
      // A list whose items are single paragraphs is tight: no paragraph margins.
      const items = b.items.map((item, n) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: items have no identity but their place
        <li key={n}>{render(item, onLink, item.length === 1 && item[0].kind === 'paragraph')}</li>
      ))
      return b.ordered ? <ol start={b.start === 1 ? undefined : b.start}>{items}</ol> : <ul>{items}</ul>
    }
  }
}

// How many levels a heading is lowered by (a chat reply's "#" shouldn't outshout the page),
// and whether a single line break in a paragraph is one (as a model means it in a reply).
const HeadingShift = createContext(0)
const LineBreaks = createContext(false)

function Paragraph({ text, onLink, tight }: { text: string; onLink?: LinkHandler; tight: boolean }) {
  // As a hard break ("  \n"), which inline() draws: only in a paragraph, never in code.
  const shown = useContext(LineBreaks) ? text.replace(/ *\n/g, '  \n') : text
  return tight ? <span>{inline(shown, onLink)}</span> : <p>{inline(shown, onLink)}</p>
}

function Heading({ level, children }: { level: number; children: ReactNode }) {
  const H = `h${Math.min(6, level + useContext(HeadingShift))}` as 'h1'
  return <H>{children}</H>
}

// headingShift lowers every heading by that many levels (to h6 at most): 2 makes "#" an h3.
// lineBreaks makes each line break in a paragraph a break, as in a chat reply.
export function Markdown({
  text,
  onLink,
  headingShift = 0,
  lineBreaks = false,
}: {
  text: string
  onLink?: LinkHandler
  headingShift?: number
  lineBreaks?: boolean
}) {
  return (
    <HeadingShift.Provider value={headingShift}>
      <LineBreaks.Provider value={lineBreaks}>
        <div className="tt-markdown">{render(parseBlocks(text), onLink)}</div>
      </LineBreaks.Provider>
    </HeadingShift.Provider>
  )
}
