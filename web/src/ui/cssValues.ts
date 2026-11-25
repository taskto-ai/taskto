// Reading the token files without a browser, for the tests: the custom properties each rule
// declares, a value resolved through var() and light-dark() for one side, and the contrast of two
// colours. It reads only what the token files write (hex and rgb()/rgba() colours); anything else
// is an error, so a theme can't slip past a check by writing a colour this doesn't understand.

export interface Rule {
  selector: string
  // The @layer the rule is in, if any.
  layer: string | undefined
  props: Map<string, string>
}

// The rules of a stylesheet with the custom properties they declare. Comments are skipped; a
// string is kept as it is.
export function rules(css: string): Rule[] {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, ' ')
  const out: Rule[] = []
  const stack: { prelude: string; rule?: Rule }[] = []
  let buf = ''
  const declare = () => {
    const rule = stack.at(-1)?.rule
    const text = buf.trim()
    buf = ''
    if (!rule || !text) return
    const colon = text.indexOf(':')
    if (colon < 0) return
    const name = text.slice(0, colon).trim()
    if (name.startsWith('--'))
      rule.props.set(
        name,
        text
          .slice(colon + 1)
          .trim()
          .replace(/\s+/g, ' '),
      )
  }
  for (let i = 0; i < source.length; i++) {
    const c = source[i]
    if (c === '"' || c === "'") {
      let j = i + 1
      while (j < source.length && source[j] !== c) j += source[j] === '\\' ? 2 : 1
      buf += source.slice(i, j + 1)
      i = j
    } else if (c === '{') {
      const prelude = buf.trim()
      buf = ''
      if (prelude.startsWith('@')) stack.push({ prelude })
      else {
        const layer = [...stack].reverse().find((s) => s.prelude.startsWith('@layer'))
        const rule: Rule = {
          selector: prelude.replace(/\s+/g, ' '),
          layer: layer?.prelude.slice('@layer'.length).trim(),
          props: new Map(),
        }
        out.push(rule)
        stack.push({ prelude, rule })
      }
    } else if (c === '}') {
      declare()
      stack.pop()
    } else if (c === ';') {
      if (stack.at(-1)?.rule) declare()
      else buf = '' // an at-rule statement: @import, @layer a, b;
    } else buf += c
  }
  return out
}

// The custom properties of all rules with this selector, later ones winning.
export function propsOf(all: Rule[], selector: string): Map<string, string> {
  const props = new Map<string, string>()
  for (const r of all) if (r.selector === selector) for (const [k, v] of r.props) props.set(k, v)
  return props
}

export type Scheme = 'light' | 'dark'

// Splits "a, b(c, d), e" at its top-level commas.
function args(text: string): string[] {
  const out: string[] = []
  let depth = 0
  let cur = ''
  for (const c of text) {
    if (c === '(') depth++
    if (c === ')') depth--
    if (c === ',' && depth === 0) {
      out.push(cur.trim())
      cur = ''
    } else cur += c
  }
  out.push(cur.trim())
  return out
}

// A value with every var() replaced from props and every light-dark() reduced to the scheme's
// side. Throws on a var() that props don't have (and that gives no fallback), or on a loop.
export function resolve(value: string, props: Map<string, string>, scheme: Scheme, seen: string[] = []): string {
  let out = value.trim()
  for (;;) {
    const m = /(var|light-dark)\(/.exec(out)
    if (!m) return out
    const open = m.index + m[0].length
    let depth = 1
    let end = open
    while (end < out.length && depth > 0) {
      if (out[end] === '(') depth++
      if (out[end] === ')') depth--
      end++
    }
    const inner = out.slice(open, end - 1)
    let replacement: string
    if (m[1] === 'light-dark') {
      const [light, dark] = args(inner)
      if (dark === undefined) throw new Error(`light-dark() needs two values: ${value}`)
      replacement = scheme === 'light' ? light : dark
    } else {
      const [name, ...fallback] = args(inner)
      if (seen.includes(name)) throw new Error(`${[...seen, name].join(' → ')} is a loop`)
      const own = props.get(name)
      if (own !== undefined) replacement = resolve(own, props, scheme, [...seen, name])
      else if (fallback.length > 0) replacement = fallback.join(', ')
      else throw new Error(`${name} is not defined${seen.length ? ` (from ${seen.join(' → ')})` : ''}`)
    }
    out = out.slice(0, m.index) + replacement + out.slice(end)
  }
}

export type RGBA = [number, number, number, number]

// A colour as the token files write it: #rgb, #rrggbb, #rrggbbaa, rgb() or rgba().
export function parseColor(text: string): RGBA {
  const t = text.trim().toLowerCase()
  let m = /^#([0-9a-f]{3})$/.exec(t)
  if (m) {
    const [r, g, b] = m[1].split('').map((h) => Number.parseInt(h + h, 16))
    return [r, g, b, 1]
  }
  m = /^#([0-9a-f]{6})([0-9a-f]{2})?$/.exec(t)
  if (m) {
    const n = (i: number) => Number.parseInt(m?.[1].slice(i, i + 2) ?? '', 16)
    return [n(0), n(2), n(4), m[2] ? Number.parseInt(m[2], 16) / 255 : 1]
  }
  m = /^rgba?\(([^)]*)\)$/.exec(t)
  if (m) {
    const parts = m[1].split(/[\s,/]+/).filter(Boolean)
    if (parts.length === 3 || parts.length === 4) {
      const [r, g, b] = parts.slice(0, 3).map(Number)
      const a =
        parts[3] === undefined ? 1 : parts[3].endsWith('%') ? Number.parseFloat(parts[3]) / 100 : Number(parts[3])
      if ([r, g, b, a].every((v) => Number.isFinite(v))) return [r, g, b, a]
    }
  }
  throw new Error(`not a colour the tokens may use: ${text}`)
}

// Colours laid one over another, top first, flattened to one opaque colour. The last must be
// opaque: a ground has to end on something.
export function flatten(stack: RGBA[]): RGBA {
  const bottom = stack.at(-1)
  if (bottom?.[3] !== 1) throw new Error('a ground must end on an opaque colour')
  let [r, g, b] = bottom as RGBA
  for (let i = stack.length - 2; i >= 0; i--) {
    const [tr, tg, tb, a] = stack[i]
    r = tr * a + r * (1 - a)
    g = tg * a + g * (1 - a)
    b = tb * a + b * (1 - a)
  }
  return [r, g, b, 1]
}

function luminance([r, g, b]: RGBA): number {
  const lin = (v: number) => {
    const s = v / 255
    return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4
  }
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b)
}

// The WCAG 2.x contrast ratio of a colour (laid over the ground if it is translucent) on an opaque
// ground.
export function contrast(fg: RGBA, ground: RGBA): number {
  const top = flatten([fg, ground])
  const [a, b] = [luminance(top), luminance(ground)].sort((x, y) => y - x)
  return (a + 0.05) / (b + 0.05)
}
