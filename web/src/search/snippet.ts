// A search result's snippet is a piece of a document's source: the server returns it as the
// file has it, in pieces that say which are the matched words. For display the Markdown marks
// (a heading's #, **bold**, `code`, a link's brackets) are noise, so they are taken out here.
// Nothing about what the server returns changes, and nothing here makes markup: the result is
// still plain text in pieces.

export type SnippetPiece = { text: string; match?: boolean }

// Wrap the matched words so a mark that spans a match (**the <match>orchard</match>**) is read
// whole, then split them out again. Private-use characters, which a document doesn't hold.
const OPEN = ''
const CLOSE = ''

const FENCE = /^[ \t]*(```|~~~).*$/gm
const HEADING = /^[ \t]{0,3}#{1,6}[ \t]+/gm
const QUOTE = /^[ \t]*(>[ \t]?)+/gm
const BULLET = /^[ \t]*[-*+][ \t]+(\[[ xX]\][ \t]+)?/gm
const IMAGE = /!\[([^\]]*)\]\([^)]*\)/g
const LINK = /\[([^\]]*)\]\([^)]*\)/g
const STRONG = /(\*\*|__)(?=\S)([\s\S]*?\S)\1/g
const STRAY_STRONG = /\*\*/g
const EMPHASIS_STAR = /\*(?=\S)([^*\n]*?\S)\*/g
const EMPHASIS_UNDERSCORE = /(^|[^\w])_(?=\S)([^_\n]*?\S)_(?!\w)/g
const STRIKE = /~~(?=\S)([\s\S]*?\S)~~/g
const CODE = /`+([^`]*?)`+/g

export function cleanSnippet(pieces: SnippetPiece[]): SnippetPiece[] {
  let text = pieces
    .map((p) => {
      const t = p.text.replaceAll(OPEN, '').replaceAll(CLOSE, '')
      return p.match ? `${OPEN}${t}${CLOSE}` : t
    })
    .join('')
  text = text
    .replace(FENCE, '')
    .replace(HEADING, '')
    .replace(QUOTE, '')
    .replace(BULLET, '')
    .replace(IMAGE, '$1')
    .replace(LINK, '$1')
    .replace(CODE, '$1')
    .replace(STRONG, '$2')
    .replace(STRAY_STRONG, '')
    .replace(EMPHASIS_STAR, '$1')
    .replace(EMPHASIS_UNDERSCORE, '$1$2')
    .replace(STRIKE, '$1')
    // Lines run together: a snippet is a line of running text.
    .replace(/\s+/g, ' ')

  const out: SnippetPiece[] = []
  let match = false
  let buffer = ''
  const flush = () => {
    if (buffer !== '') out.push(match ? { text: buffer, match: true } : { text: buffer })
    buffer = ''
  }
  for (const ch of text) {
    if (ch === OPEN || ch === CLOSE) {
      flush()
      match = ch === OPEN
    } else buffer += ch
  }
  flush()

  // No leading or trailing blank from a stripped line break.
  if (out.length > 0) out[0] = { ...out[0], text: out[0].text.trimStart() }
  const last = out.length - 1
  if (last >= 0) out[last] = { ...out[last], text: out[last].text.trimEnd() }
  return out.filter((p) => p.text !== '')
}
