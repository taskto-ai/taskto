#!/usr/bin/env node

import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

export const UNSTYLED = new Map([
  [
    'ui-control',
    "marks a control's native element (Button, Input, Checkbox …); like every ui- class it keeps the old element rules off, and no rule needs it on its own",
  ],
])

const CLASS = /(?:tt|ui)-[a-z0-9]+(?:-[a-z0-9]+)*/g
const CLASS_SELECTOR = /\.((?:tt|ui)-[a-z0-9]+(?:-[a-z0-9]+)*)(?![\w-])/g

export function classSelectors(prelude) {
  const bare = prelude.replace(/\[[^\]]*\]/g, '').replace(/"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'/g, '""')
  return [...bare.matchAll(CLASS_SELECTOR)].map((m) => m[1])
}

export function scan(css) {
  const top = []
  const mentioned = new Set()
  const declarations = []
  const blocks = []
  let depth = 0
  let prelude = ''
  let preludeLine = 1
  let line = 1
  for (let i = 0; i < css.length; i++) {
    const c = css[i]
    if (c === '\n') line++
    if (c === '/' && css[i + 1] === '*') {
      const end = css.indexOf('*/', i + 2)
      const stop = end === -1 ? css.length : end + 2
      for (const ch of css.slice(i, stop)) if (ch === '\n') line++
      i = stop - 1
      continue
    }
    if (c === '"' || c === "'") {
      let j = i + 1
      while (j < css.length && css[j] !== c) j += css[j] === '\\' ? 2 : 1
      prelude += css.slice(i, j + 1)
      i = j
      continue
    }
    if (c === '{') {
      const text = prelude.trim()
      const isAt = text.startsWith('@')
      for (const name of classSelectors(text)) mentioned.add(name)
      if (!isAt && blocks.every((b) => b === 'layer')) {
        for (const sel of splitSelectors(text)) top.push({ selector: sel, line: preludeLine })
      }
      blocks.push(isAt ? (/^@layer\b/.test(text) ? 'layer' : 'at') : 'rule')
      depth++
      prelude = ''
      continue
    }
    if (c === '}') {
      declaration(prelude, preludeLine)
      blocks.pop()
      depth = Math.max(0, depth - 1)
      prelude = ''
      continue
    }
    if (c === ';') {
      declaration(prelude, preludeLine)
      prelude = ''
      continue
    }
    if (prelude.trim() === '') preludeLine = line
    prelude += c
  }
  return { top, mentioned, declarations }

  function declaration(text, at) {
    if (blocks.length === 0) return
    const colon = text.indexOf(':')
    if (colon < 0) return
    const property = text.slice(0, colon).trim()
    if (!/^-{0,2}[a-zA-Z][\w-]*$/.test(property)) return
    declarations.push({ property, value: text.slice(colon + 1).trim(), line: at })
  }
}

const NAMED_COLORS = new Set(
  (
    'aliceblue antiquewhite aqua aquamarine azure beige bisque black blanchedalmond blue blueviolet brown ' +
    'burlywood cadetblue chartreuse chocolate coral cornflowerblue cornsilk crimson cyan darkblue darkcyan ' +
    'darkgoldenrod darkgray darkgreen darkgrey darkkhaki darkmagenta darkolivegreen darkorange darkorchid ' +
    'darkred darksalmon darkseagreen darkslateblue darkslategray darkslategrey darkturquoise darkviolet ' +
    'deeppink deepskyblue dimgray dimgrey dodgerblue firebrick floralwhite forestgreen fuchsia gainsboro ' +
    'ghostwhite gold goldenrod gray green greenyellow grey honeydew hotpink indianred indigo ivory khaki ' +
    'lavender lavenderblush lawngreen lemonchiffon lightblue lightcoral lightcyan lightgoldenrodyellow ' +
    'lightgray lightgreen lightgrey lightpink lightsalmon lightseagreen lightskyblue lightslategray ' +
    'lightslategrey lightsteelblue lightyellow lime limegreen linen magenta maroon mediumaquamarine ' +
    'mediumblue mediumorchid mediumpurple mediumseagreen mediumslateblue mediumspringgreen ' +
    'mediumturquoise mediumvioletred midnightblue mintcream mistyrose moccasin navajowhite navy oldlace ' +
    'olive olivedrab orange orangered orchid palegoldenrod palegreen paleturquoise palevioletred ' +
    'papayawhip peachpuff peru pink plum powderblue purple rebeccapurple red rosybrown royalblue ' +
    'saddlebrown salmon sandybrown seagreen seashell sienna silver skyblue slateblue slategray slategrey ' +
    'snow springgreen steelblue tan teal thistle tomato turquoise violet wheat white whitesmoke yellow ' +
    'yellowgreen ' +
    'accentcolor accentcolortext activetext buttonborder buttonface buttontext canvas canvastext field ' +
    'fieldtext graytext highlight highlighttext linktext mark marktext selecteditem selecteditemtext ' +
    'visitedtext activeborder activecaption appworkspace background buttonhighlight buttonshadow ' +
    'captiontext inactiveborder inactivecaption inactivecaptiontext infobackground infotext menu menutext ' +
    'scrollbar threeddarkshadow threedface threedhighlight threedlightshadow threedshadow window ' +
    'windowframe windowtext'
  ).split(' '),
)
const COLOR_FUNCTION = /(?<![\w-])(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\(/i
const HEX = /#[0-9a-fA-F]{3,8}(?![\w-])/

const IDENTIFIER_PROPERTIES = new Set([
  'animation',
  'animation-name',
  'font',
  'font-family',
  'grid-area',
  'grid-column',
  'grid-row',
  'grid-template',
  'grid-template-areas',
  'list-style',
  'list-style-type',
  'counter-reset',
  'counter-increment',
  'counter-set',
  'container',
  'container-name',
  'view-transition-name',
  'transition',
  'transition-property',
  'will-change',
])

function functionArguments(value) {
  const stack = []
  let out = ''
  for (let i = 0; i < value.length; i++) {
    const c = value[i]
    if (c === ')') stack.pop()
    const inside = stack.length > 0 && !stack[0]
    out += inside && c !== '(' ? c : ' '
    if (c === '(') stack.push(/(?<![\w-])var$/i.test(value.slice(0, i)))
  }
  return out
}

export function colorLiteral(value, property = '') {
  const bare = value
    .replace(/url\((?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^)]*)\)/gi, 'url()')
    .replace(/"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'/g, '""')
    .replace(/--[\w-]+/g, '--x')
  const hex = HEX.exec(bare)
  if (hex) return hex[0]
  const fn = COLOR_FUNCTION.exec(bare)
  if (fn) return `${fn[0]})`
  const words = IDENTIFIER_PROPERTIES.has(property.toLowerCase()) ? functionArguments(bare) : bare
  for (const word of words.matchAll(/(?<![\w-])[a-zA-Z]+(?![\w-])/g)) {
    if (NAMED_COLORS.has(word[0].toLowerCase())) return word[0]
  }
  return undefined
}

function splitSelectors(text) {
  const out = []
  let depth = 0
  let cur = ''
  for (const c of text) {
    if (c === '(') depth++
    if (c === ')') depth--
    if (c === ',' && depth === 0) {
      out.push(cur)
      cur = ''
    } else cur += c
  }
  out.push(cur)
  return out.map((s) => s.replace(/\s+/g, ' ').trim()).filter(Boolean)
}

export function classesUsed(raw) {
  const source = blankComments(raw)
  const used = new Map()
  for (const m of source.matchAll(/\bclassName\s*=\s*/g)) {
    const start = m.index + m[0].length
    const end = expressionEnd(source, start)
    for (const c of source.slice(start, end).matchAll(CLASS)) {
      if (/[\w-]/.test(source[start + c.index - 1] ?? '')) continue
      if (!used.has(c[0])) used.set(c[0], start + c.index)
    }
  }
  tableClasses(source, used)
  return used
}

export function blankComments(source) {
  let out = ''
  for (let i = 0; i < source.length; ) {
    const c = source[i]
    let end = i + 1
    if (c === '"' || c === "'") end = skipString(source, i, c) + 1
    else if (c === '`') end = skipTemplate(source, i) + 1
    else if (c === '/' && source[i + 1] === '/') {
      end = source.indexOf('\n', i)
      if (end < 0) end = source.length
      out += ' '.repeat(end - i)
      i = end
      continue
    } else if (c === '/' && source[i + 1] === '*') {
      const close = source.indexOf('*/', i + 2)
      end = close < 0 ? source.length : close + 2
      out += source.slice(i, end).replace(/[^\n]/g, ' ')
      i = end
      continue
    }
    out += source.slice(i, end)
    i = end
  }
  return out
}

function tableClasses(source, used) {
  for (const m of source.matchAll(/\bconst\s+(?:[A-Z][A-Z0-9_]*\b[^=\n]*=\s*(?=[{[])|[a-z]\w*\b[^=\n]*=\s*(?=\[))/g)) {
    const start = m.index + m[0].length
    const end = tableEnd(source, start)
    for (const lit of source
      .slice(start, end)
      .matchAll(/(["'])((?:tt|ui)-[a-z0-9]+(?:-[a-z0-9]+)*(?: (?:tt|ui)-[a-z0-9]+(?:-[a-z0-9]+)*)*)\1/g)) {
      for (const c of lit[2].matchAll(CLASS)) {
        const at = start + lit.index + 1 + c.index
        if (!used.has(c[0])) used.set(c[0], at)
      }
    }
  }
}

function tableEnd(s, start) {
  let depth = 0
  for (let i = start; i < s.length; i++) {
    const c = s[i]
    if (c === '"' || c === "'") i = skipString(s, i, c)
    else if (c === '`') i = skipTemplate(s, i)
    else if (c === '{' || c === '[') depth++
    else if ((c === '}' || c === ']') && --depth === 0) return i + 1
  }
  return s.length
}

function expressionEnd(s, start) {
  const open = s[start]
  if (open === '"' || open === "'") {
    const close = s.indexOf(open, start + 1)
    return close === -1 ? s.length : close + 1
  }
  if (open !== '{') return start
  let depth = 0
  for (let i = start; i < s.length; i++) {
    const c = s[i]
    if (c === '"' || c === "'") {
      i = skipString(s, i, c)
    } else if (c === '`') {
      i = skipTemplate(s, i)
    } else if (c === '{') depth++
    else if (c === '}' && --depth === 0) return i + 1
  }
  return s.length
}

function skipString(s, i, q) {
  let j = i + 1
  while (j < s.length && s[j] !== q) j += s[j] === '\\' ? 2 : 1
  return j
}

function skipTemplate(s, i) {
  let j = i + 1
  while (j < s.length && s[j] !== '`') {
    if (s[j] === '\\') j += 2
    else if (s[j] === '$' && s[j + 1] === '{') {
      let depth = 0
      for (j += 1; j < s.length; j++) {
        if (s[j] === '`') j = skipTemplate(s, j)
        else if (s[j] === '"' || s[j] === "'") j = skipString(s, j, s[j])
        else if (s[j] === '{') depth++
        else if (s[j] === '}' && --depth === 0) break
      }
      j++
    } else j++
  }
  return j
}

function components(dir, out = []) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) components(path, out)
    else if (/\.tsx?$/.test(entry.name) && !/\.(test|spec)\.tsx?$/.test(entry.name)) out.push(path)
  }
  return out
}

function stylesheets(dir, out = []) {
  for (const entry of readdirSync(dir, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) stylesheets(path, out)
    else if (entry.name.endsWith('.css')) out.push(path)
  }
  return out
}

const THEMES = 'src/ui/themes/'

export const OLD_STYLESHEETS = ['ui/base.css', 'legacy-shared.css', 'markdown.legacy.css']
const LIST = 'src/styles.css'
export const LAYERS = ['tokens', 'themes', 'base', 'components', 'screens']
const ENTRY = 'src/main.tsx'
const DESIGN_ENTRY = 'src/ui/index.css'
const BASE = 'src/ui/base.css'

export function blankCssComments(css) {
  let out = ''
  for (let i = 0; i < css.length; ) {
    const c = css[i]
    if (c === '"' || c === "'") {
      let j = i + 1
      while (j < css.length && css[j] !== c) j += css[j] === '\\' ? 2 : 1
      out += css.slice(i, j + 1)
      i = j + 1
    } else if (c === '/' && css[i + 1] === '*') {
      const close = css.indexOf('*/', i + 2)
      const end = close < 0 ? css.length : close + 2
      out += css.slice(i, end).replace(/[^\n]/g, ' ')
      i = end
    } else {
      out += c
      i++
    }
  }
  return out
}

export function topLevel(css) {
  const text = blankCssComments(css)
  const out = []
  let i = 0
  while (i < text.length) {
    if (/\s/.test(text[i])) {
      i++
      continue
    }
    let j = i
    while (j < text.length && text[j] !== ';' && text[j] !== '{') {
      if (text[j] === '"' || text[j] === "'") {
        const q = text[j]
        j++
        while (j < text.length && text[j] !== q) j += text[j] === '\\' ? 2 : 1
      }
      j++
    }
    const prelude = text.slice(i, j).trim().replace(/\s+/g, ' ')
    const line = text.slice(0, i).split('\n').length
    if (text[j] === '{') {
      let depth = 0
      for (; j < text.length; j++) {
        const c = text[j]
        if (c === '\\') j++
        else if (c === '"' || c === "'") {
          j++
          while (j < text.length && text[j] !== c) j += text[j] === '\\' ? 2 : 1
        } else if (c === '{') depth++
        else if (c === '}' && --depth === 0) break
      }
      out.push({ kind: 'block', prelude, line, start: i, end: j + 1 })
    } else {
      const m = /^@import\s+(?:url\(\s*(?:["']([^"']+)["']|([^)\s"']+))\s*\)|["']([^"']+)["'])\s*(.*)$/i.exec(prelude)
      out.push(
        m
          ? { kind: 'import', url: m[1] ?? m[2] ?? m[3], rest: m[4], line, start: i, end: j + 1 }
          : { kind: 'statement', prelude, line, start: i, end: j + 1 },
      )
    }
    i = j + 1
  }
  return out
}

function reachable(web) {
  const seen = new Set()
  const todo = [join(web, ENTRY)]
  const resolveModule = (from, spec) => {
    const base = join(dirname(from), spec)
    for (const p of [base, `${base}.tsx`, `${base}.ts`, join(base, 'index.tsx'), join(base, 'index.ts')]) {
      if (/\.tsx?$/.test(p) && existsSync(p)) return p
    }
    return undefined
  }
  while (todo.length > 0) {
    const path = todo.pop()
    if (seen.has(path)) continue
    seen.add(path)
    const source = blankComments(readFileSync(path, 'utf8'))
    for (const m of source.matchAll(/(?:\bfrom\s+|\bimport\s*\(\s*|\bimport\s+)["'](\.{1,2}\/[^"']+)["']/g)) {
      if (m[1].endsWith('.css')) continue
      const next = resolveModule(path, m[1].replace(/\?.*$/, ''))
      if (next && !seen.has(next)) todo.push(next)
    }
    for (const m of source.matchAll(/\bimport\.meta\.glob(?:<[^>]*>)?\(\s*["'](\.{1,2}\/[^"']*)["']/g)) {
      const dir = join(dirname(path), dirname(m[1]))
      const name = m[1].split('/').pop()
      const pattern = new RegExp(`^${name.replace(/[.+^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '[^/]*')}$`)
      if (!existsSync(dir)) continue
      for (const f of readdirSync(dir)) if (pattern.test(f) && /\.tsx?$/.test(f)) todo.push(join(dir, f))
    }
  }
  return seen
}

function moduleImports(web) {
  const out = new Map()
  const live = reachable(web)
  for (const path of components(join(web, 'src'))) {
    if (!live.has(path)) continue
    const source = blankComments(readFileSync(path, 'utf8'))
    for (const m of source.matchAll(/\bimport\s+["'](\.{1,2}\/[^"'?]+\.css)["']/g)) {
      const file = relative(web, join(dirname(path), m[1]))
        .split('\\')
        .join('/')
      if (!out.has(file)) out.set(file, [])
      out.get(file).push(relative(web, path).split('\\').join('/'))
    }
  }
  return out
}

function cssImports(web, file) {
  if (!existsSync(join(web, file))) return []
  return topLevel(readFileSync(join(web, file), 'utf8'))
    .filter((s) => s.kind === 'import')
    .map((s) => ({
      ...s,
      file: relative(web, join(web, dirname(file), s.url))
        .split('\\')
        .join('/'),
    }))
}

export function oldStylesheets(web) {
  return cssImports(web, LIST)
    .filter((s) => existsSync(join(web, s.file)))
    .map((s) => ({ file: s.file, css: readFileSync(join(web, s.file), 'utf8') }))
}

function loadingErrors(web, sheets) {
  const errors = []
  const entry = blankComments(readFileSync(join(web, ENTRY), 'utf8'))
  const entryImports = [...entry.matchAll(/\bimport\s+(?:[^'"]*?\bfrom\s+)?["']([^"']+)["']/g)].map((m) => m[1])
  const first = entryImports.length > 0 ? [null, entryImports[0]] : null
  if (first?.[1] !== './ui/index.css') {
    errors.push(
      `${ENTRY}: its first import is ${first ? `'${first[1]}'` : 'missing'}, not './ui/index.css': the build emits CSS in import order, so a component's @layer block would come before the layers' order is declared, and @layer base would beat @layer components`,
    )
  }

  const design = existsSync(join(web, DESIGN_ENTRY)) ? topLevel(readFileSync(join(web, DESIGN_ENTRY), 'utf8')) : []
  if (design[0]?.kind !== 'statement' || design[0].prelude !== `@layer ${LAYERS.join(', ')}`) {
    errors.push(
      `${DESIGN_ENTRY}: its first statement is \`@layer ${LAYERS.join(', ')};\` (the order index.html declares too), not \`${design[0]?.prelude ?? design[0]?.url ?? 'nothing'}\``,
    )
  }
  for (const s of design) {
    if (s.kind === 'import' && s.rest) {
      errors.push(
        `${DESIGN_ENTRY}:${s.line}: imports ${s.url} with \`${s.rest}\`: the design system's files load on every page, unconditionally`,
      )
    }
  }

  const list = existsSync(join(web, LIST)) ? topLevel(readFileSync(join(web, LIST), 'utf8')) : []
  for (const s of list) {
    if (s.kind !== 'import')
      errors.push(`${LIST}:${s.line}: holds only @imports of the old stylesheets, not \`${s.prelude}\``)
    else if (s.rest)
      errors.push(
        `${LIST}:${s.line}: imports ${s.url} with \`${s.rest}\`: the old stylesheets are imported plainly, their layering is in the files`,
      )
  }
  const listed = cssImports(web, LIST).map((s) => s.file)
  const expected = OLD_STYLESHEETS.map((f) => `src/${f}`)
  if (listed.join('\n') !== expected.join('\n')) {
    errors.push(
      `${LIST}: lists ${listed.map((f) => f.slice(4)).join(', ') || 'nothing'}; the old stylesheets are, in this order: ${OLD_STYLESHEETS.join(', ')}. The order is the cascade: change it only with OLD_STYLESHEETS, after checking which rules meet (docs/design.md)`,
    )
  }
  for (const f of listed) if (!sheets.includes(f)) errors.push(`${LIST}: lists ${f}, which is not there`)

  const ways = new Map(sheets.map((f) => [f, []]))
  const add = (f, kind, way) => ways.get(f)?.push({ kind, way })
  if (!entryImports.includes('./styles.css')) {
    errors.push(`${ENTRY}: doesn't import './styles.css', so no old stylesheet it lists is loaded`)
  }
  for (const f of listed) add(f, 'list', `listed in ${LIST}`)
  for (const s of cssImports(web, DESIGN_ENTRY)) add(s.file, 'index', `imported by ${DESIGN_ENTRY}`)
  for (const [f, modules] of moduleImports(web)) {
    const others = modules.filter((m) => m !== ENTRY)
    if (others.length > 0) add(f, 'module', `imported by ${others.join(', ')}`)
    else if (f !== LIST && f !== DESIGN_ENTRY) add(f, 'module', `imported by ${ENTRY}`)
  }
  ways.delete(LIST)
  ways.delete(DESIGN_ENTRY)
  for (const [f, list] of ways) {
    const how = list.map((w) => w.way)
    const kinds = new Set(list.map((w) => w.kind))
    if (how.length === 0) {
      errors.push(
        `${f}: nothing loads it: an old screen's rules are listed in ${LIST} (OLD_STYLESHEETS); a component's or a new screen's stylesheet is imported by its module`,
      )
    } else if (kinds.size > 1 || list.filter((w) => w.kind !== 'module').length > 1) {
      errors.push(`${f}: loaded more than one way (${how.join('; ')}): one is its place in the cascade`)
    }
  }

  for (const f of listed) {
    if (f === BASE || !existsSync(join(web, f))) continue
    const text = blankCssComments(readFileSync(join(web, f), 'utf8')).replace(
      /"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'/g,
      (q) => q.replace(/[^\n]/g, ' '),
    )
    for (const m of text.matchAll(/@layer\b[^{;]*/g)) {
      const line = text.slice(0, m.index).split('\n').length
      errors.push(
        `${f}:${line}: declares \`${m[0].trim()}\`: the old screens' rules are in no layer until the screen is rebuilt, and the move is checked rule by rule`,
      )
    }
  }
  if (existsSync(join(web, BASE))) errors.push(...baseErrors(readFileSync(join(web, BASE), 'utf8')))
  return errors
}

function baseErrors(css) {
  const errors = []
  const parts = topLevel(css)
  const shape = parts.map((s) => s.prelude ?? `@import ${s.url}`)
  const last = parts.at(-1)
  if (
    shape[0] !== '@layer base' ||
    parts[0]?.kind !== 'block' ||
    last?.kind !== 'block' ||
    shape.filter((p) => /^@layer\b/.test(p)).length !== 1 ||
    shape.at(-1) !== '@media (pointer: coarse)' ||
    parts.length < 2
  ) {
    errors.push(
      `${BASE}: is one \`@layer base\` block, then in no layer the rules that reach every element on purpose, then the touch screen's \`@media (pointer: coarse)\` block, not: ${shape.join(' | ')}`,
    )
    return errors
  }
  const lineOf = (offset) => css.slice(0, offset).split('\n').length - 1
  const middle = css.slice(parts[0].end, last.start)
  for (const rule of allRules(middle)) {
    const listed = EVERYWHERE.get(rule.selector)
    if (!listed || !rule.properties.every((p) => listed.properties.includes(p))) {
      errors.push(
        `${BASE}:${lineOf(parts[0].end) + rule.line}: \`${rule.selector}\` is in no layer: only what reaches every element on purpose (EVERYWHERE) is; put it in @layer base`,
      )
    }
  }
  for (const rule of allRules(css.slice(last.start, last.end))) {
    if (!optsOut(compounds(rule.selector).at(-1) ?? '')) {
      errors.push(
        `${BASE}:${lineOf(last.start) + rule.line}: \`${rule.selector}\` is in no layer and doesn't leave the components' elements out: give it ${OPT_OUT}`,
      )
    }
  }
  return errors
}

const TOKENS_ONLY = 'src/ui/'
const DEFINE_TOKENS = ['src/ui/tokens.css', 'src/ui/legacy.css', THEMES]

export const SCALE_LITERALS = new Map([
  ['0', 'nothing: no space, no radius, no time (with or without a unit)'],
  ['1px', 'a hairline: a border-wide offset, finer than the smallest step'],
  ['-1px', 'a hairline the other way: pulls a box back by a border, as the visually hidden pattern does'],
  ['100%', 'the whole of the box'],
  ['50%', 'half of the box: the radius of a circle'],
])

const SCALE_PROPERTY =
  /^(font|font-size|margin(-.+)?|padding(-.+)?|scroll-margin(-.+)?|scroll-padding(-.+)?|gap|row-gap|column-gap|grid-gap|grid-row-gap|grid-column-gap|border(-.+)?-radius|(-webkit-)?box-shadow|text-shadow|(-webkit-)?transition(-duration|-delay)?|(-webkit-)?animation(-duration|-delay)?)$/i

export const LOCAL_LITERALS = new Map([
  [
    '--a2ui-button-padding',
    "the apps renderer's own variable: it takes a padding in its units, and the app's buttons are 0.4rem 1rem, off the space scale",
  ],
  [
    '--a2ui-choicepicker-chip-padding',
    "the apps renderer's own variable: a chip's padding as the app's chips have it, 0.3rem 0.85rem, off the space scale",
  ],
  [
    '--a2ui-choicepicker-chip-font-size',
    "the apps renderer's own variable: a chip's text as the app's chips have it, in rem so it follows the reader's text size",
  ],
])

export function scaleLiteral(value, property) {
  if (property.toLowerCase() === 'initial-value') property = '--registered'
  if (!property.startsWith('--') && !SCALE_PROPERTY.test(property)) return undefined
  if (property.startsWith('--') && LOCAL_LITERALS.has(property)) return undefined
  let bare = value
    .replace(/url\((?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^)]*)\)/gi, 'url()')
    .replace(/"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'/g, '""')
  for (let prev = ''; prev !== bare; ) {
    prev = bare
    bare = bare.replace(
      /(?<![\w-])(?:var\(\s*--[\w-]+|env\(\s*[\w-]+(?:\s+\d+)*)\s*(?:,([^()]*))?\)/gi,
      (_, fallback) => ` ${fallback ?? ''} `,
    )
  }
  for (const m of bare.matchAll(/(?<![\w.#-])-?(?:\d+\.?\d*|\.\d+)(?:e-?\d+)?([a-z%]+)(?![\w-])/gi)) {
    const literal = m[0].toLowerCase()
    if (/^-?0*\.?0*[a-z%]+$/.test(literal)) continue
    if (property.startsWith('--') && literal.endsWith('%')) continue
    if (SCALE_LITERALS.has(literal)) continue
    return m[0]
  }
  return undefined
}

export const OPT_OUT = ':not(:where([class^="ui-"], [class*=" ui-"]))'

export const WRAPPERS = new Set(['#root', '#tt-main'])

export const EVERYWHERE = new Map([
  ['html', { properties: ['height', '-webkit-text-size-adjust'], reason: 'the page itself: no component draws it' }],
  [
    'body',
    {
      properties: [
        'height',
        'margin',
        'background',
        'color',
        'font',
        '-webkit-font-smoothing',
        'text-rendering',
        'overscroll-behavior-y',
        'touch-action',
      ],
      reason: 'the page itself: no component draws it, and its font and colour are what everything inherits',
    },
  ],
  [
    'html[data-keyboard="open"] body',
    {
      properties: ['height', 'min-height', 'max-height', 'overflow'],
      reason: "the page itself, while a phone's keyboard is open",
    },
  ],
  [':root', { properties: [], reason: 'the page itself' }],
  [
    '*',
    {
      properties: ['box-sizing', 'scrollbar-color'],
      reason: "border-box sizing and the scrollbars' colours, wanted on every element",
    },
  ],
  ['*::before', { properties: ['box-sizing'], reason: 'border-box sizing, wanted on every element' }],
  ['*::after', { properties: ['box-sizing'], reason: 'border-box sizing, wanted on every element' }],
  ['::selection', { properties: ['background', 'color'], reason: 'the selection in the palette, wanted everywhere' }],
  [
    'input',
    {
      properties: ['caret-color', 'font-size'],
      reason: "every field: the caret in the text's colour, and 16px on a narrow screen so iOS doesn't zoom in",
    },
  ],
  [
    'textarea',
    {
      properties: ['caret-color', 'font-size'],
      reason: "every field: the caret in the text's colour, and 16px on a narrow screen so iOS doesn't zoom in",
    },
  ],
  ['select', { properties: ['font-size'], reason: "every field: 16px on a narrow screen so iOS doesn't zoom in" }],
  [
    'input[type="search"]::-webkit-search-cancel-button',
    {
      properties: [
        '-webkit-appearance',
        'appearance',
        'inline-size',
        'block-size',
        'cursor',
        'background-color',
        '-webkit-mask',
        'mask',
      ],
      reason: "every search field's clear button, in the palette",
    },
  ],
  [
    'input[type="search"]::-webkit-search-cancel-button:hover',
    { properties: ['background-color'], reason: "every search field's clear button, in the palette" },
  ],
])

export function nestedSelectors(parents, child) {
  const texts = ['']
  const glued = []
  for (let i = 0; i < child.length; i++) {
    const c = child[i]
    if (c === '\\') {
      texts[texts.length - 1] += child.slice(i, i + 2)
      i++
    } else if (c === '"' || c === "'") {
      let j = i + 1
      while (j < child.length && child[j] !== c) j += child[j] === '\\' ? 2 : 1
      texts[texts.length - 1] += child.slice(i, j + 1)
      i = j
    } else if (c === '&') {
      glued.push(/[^\s>+~(]$/.test(texts.at(-1)))
      texts.push('')
    } else texts[texts.length - 1] += c
  }
  if (glued.length === 0 || /^\s*[>+~]/.test(child)) {
    texts.unshift('')
    texts[1] = ` ${texts[1].trimStart()}`
    glued.unshift(false)
  }
  let out = [texts[0]]
  glued.forEach((isGlued, k) => {
    out = out.flatMap((sofar) => parents.map((p) => sofar + (isGlued ? `:is(${p})` : p) + texts[k + 1]))
  })
  return out
}

export function nestedSelector(parent, child) {
  return nestedSelectors([parent], child)[0]
}

function enclosingRule(blocks) {
  for (let i = blocks.length - 1; i >= 0; i--) {
    if (blocks[i].kind === 'rule') return blocks[i]
    if (blocks[i].kind === 'keyframes') return null
  }
  return null
}

export function allRules(css) {
  const out = []
  const blocks = []
  let prelude = ''
  let preludeLine = 1
  let line = 1
  const declare = (text) => {
    const top = blocks.at(-1)?.kind === 'at' ? enclosingRule(blocks) : blocks.at(-1)
    if (top?.kind !== 'rule') return
    const colon = text.indexOf(':')
    if (colon < 0) return
    const property = text.slice(0, colon).trim().toLowerCase()
    if (/^-{0,2}[a-z][\w-]*$/.test(property)) for (const r of top.rules) r.properties.push(property)
  }
  for (let i = 0; i < css.length; i++) {
    const c = css[i]
    if (c === '\n') line++
    if (c === '/' && css[i + 1] === '*') {
      const end = css.indexOf('*/', i + 2)
      const stop = end === -1 ? css.length : end + 2
      for (const ch of css.slice(i, stop)) if (ch === '\n') line++
      i = stop - 1
      continue
    }
    if (c === '"' || c === "'") {
      let j = i + 1
      while (j < css.length && css[j] !== c) j += css[j] === '\\' ? 2 : 1
      prelude += css.slice(i, j + 1)
      i = j
      continue
    }
    if (c === '{') {
      const text = prelude.trim()
      if (text.startsWith('@')) blocks.push({ kind: /^@(-[a-z]+-)?keyframes\b/.test(text) ? 'keyframes' : 'at' })
      else {
        const rules = []
        if (!blocks.some((b) => b.kind === 'keyframes')) {
          const parents = enclosingRule(blocks)?.rules.map((r) => r.selector)
          for (const own of splitSelectors(text)) {
            for (const selector of parents ? nestedSelectors(parents, own) : [own]) {
              const r = { selector, line: preludeLine, properties: [] }
              rules.push(r)
              out.push(r)
            }
          }
        }
        blocks.push({ kind: 'rule', rules })
      }
      prelude = ''
      continue
    }
    if (c === ';') {
      declare(prelude)
      prelude = ''
      continue
    }
    if (c === '}') {
      declare(prelude)
      blocks.pop()
      prelude = ''
      continue
    }
    if (prelude.trim() === '') preludeLine = line
    prelude += c
  }
  return out
}

export function compounds(selector) {
  const out = []
  let depth = 0
  let cur = ''
  for (const c of selector) {
    if (c === '(' || c === '[') depth++
    if (c === ')' || c === ']') depth--
    if (depth === 0 && /[\s>+~]/.test(c)) {
      if (cur) out.push(cur)
      cur = ''
    } else cur += c
  }
  if (cur) out.push(cur)
  return out
}

function own(compound) {
  let out = ''
  let depth = 0
  for (const c of compound) {
    if (c === '(' || c === '[') depth++
    if (depth === 0) out += c
    if (c === ')' || c === ']') depth--
  }
  return out
}

const scopes = (compound) => own(compound).match(/[.#][\w-]+/g) ?? []

export function reachesAnywhere(selector) {
  const parts = compounds(selector)
  const subject = parts.at(-1) ?? ''
  if (scopes(subject).length > 0) return false
  return parts.slice(0, -1).every((p) => scopes(p).every((s) => WRAPPERS.has(s)))
}

export function optsOut(compound) {
  let depth = 0
  for (let i = 0; i < compound.length; i++) {
    if (depth === 0 && compound.startsWith(OPT_OUT, i)) return true
    const c = compound[i]
    if (c === '(' || c === '[') depth++
    if (c === ')' || c === ']') depth--
  }
  return false
}

export function allowedEverywhere({ selector, properties }) {
  if (optsOut(compounds(selector).at(-1) ?? '')) return true
  const allowed = EVERYWHERE.get(selector)
  return Boolean(allowed && properties.every((p) => allowed.properties.includes(p)))
}

function everywhereErrors(file, css) {
  const errors = []
  for (const rule of allRules(css)) {
    if (!reachesAnywhere(rule.selector) || allowedEverywhere(rule)) continue
    const listed = EVERYWHERE.get(rule.selector)
    const extra = listed ? rule.properties.filter((p) => !listed.properties.includes(p)) : []
    errors.push(
      listed
        ? `${file}:${rule.line}: \`${rule.selector}\` reaches every element, and EVERYWHERE allows it only ${listed.properties.join(', ')}, not ${extra.join(', ')}: give the rest ${OPT_OUT}`
        : `${file}:${rule.line}: \`${rule.selector}\` reaches into the design system's components: give its subject ${OPT_OUT}, or list it in EVERYWHERE with the properties and the reason`,
    )
  }
  return errors
}

export function check(web) {
  const errors = []
  const mentioned = new Set()
  const seen = new Map()
  const sheets = stylesheets(join(web, 'src')).map((p) => relative(web, p).split('\\').join('/'))
  if (existsSync(join(web, ENTRY))) errors.push(...loadingErrors(web, sheets))
  for (const path of stylesheets(join(web, 'src'))) {
    const file = relative(web, path).split('\\').join('/')
    const sheet = scan(readFileSync(path, 'utf8'))
    for (const name of sheet.mentioned) mentioned.add(name)
    for (const { selector, line } of sheet.top) {
      if (!/\.(?:tt|ui)-/.test(selector)) continue
      const first = seen.get(selector)
      if (first) {
        const where = first.file === file ? `line ${first.line}` : `${first.file}:${first.line}`
        errors.push(
          `${file}:${line}: ${selector} is declared again (first at ${where}): merge the two, or give one its own class`,
        )
      } else seen.set(selector, { file, line })
    }
    if (!file.startsWith('src/ui/') || file === BASE) errors.push(...everywhereErrors(file, readFileSync(path, 'utf8')))
    if (file.startsWith(THEMES)) continue
    const tokensOnly = file.startsWith(TOKENS_ONLY)
    const definesTokens = DEFINE_TOKENS.some((d) => file === d || (d.endsWith('/') && file.startsWith(d)))
    for (const { property, value, line } of sheet.declarations) {
      const definition = property.startsWith('--') || property.toLowerCase() === 'initial-value'
      const scale = tokensOnly && !(definesTokens && definition) && scaleLiteral(value, property)
      if (scale) {
        errors.push(
          `${file}:${line}: ${property} has the literal ${scale}: use a token from src/ui/tokens.css (var(--space-…), var(--text-…), var(--radius-…), var(--shadow-…), var(--duration-…)), or add one`,
        )
      }
      const literal = colorLiteral(value, property)
      if (literal) {
        errors.push(
          `${file}:${line}: ${property} has the colour literal ${literal}: use a colour role, var(--color-…), or add one to the themes in ${THEMES}`,
        )
      }
    }
  }

  const unstyled = new Map()
  for (const file of components(join(web, 'src'))) {
    const source = readFileSync(file, 'utf8')
    for (const [cls, offset] of classesUsed(source)) {
      if (mentioned.has(cls) || UNSTYLED.has(cls) || unstyled.has(cls)) continue
      const line = source.slice(0, offset).split('\n').length
      unstyled.set(cls, `${relative(web, file)}:${line}`)
    }
  }
  for (const [cls, where] of unstyled) {
    errors.push(`${where}: ${cls} is used but no rule in the stylesheets has it: style it, or remove it`)
  }
  for (const cls of UNSTYLED.keys()) {
    if (mentioned.has(cls)) errors.push(`${cls} is in the allowlist but now has a rule: remove it from the allowlist`)
  }
  return errors
}

export const BUILT_FROM_PIECES = new Map([])

function outsideHtmlComments(html) {
  let out = ''
  let at = 0
  for (;;) {
    const open = html.indexOf('<!--', at)
    if (open < 0) return out + html.slice(at)
    out += html.slice(at, open)
    const close = html.indexOf('-->', open + 4)
    if (close < 0) return out
    at = close + 3
  }
}

function usersCode(web) {
  const dirs = [join(web, 'src'), join(web, 'e2e')].filter((d) => existsSync(d))
  const code = dirs
    .flatMap((d) => components(d))
    .filter((f) => !/\.gallery\.tsx?$/.test(f))
    .map((f) => blankComments(readFileSync(f, 'utf8')))
  const html = join(web, 'index.html')
  if (existsSync(html)) code.push(outsideHtmlComments(readFileSync(html, 'utf8')))
  return code
}

export function unusedClassErrors(web, built = BUILT_FROM_PIECES) {
  const src = join(web, 'src')
  const used = new Set(
    usersCode(web).flatMap((c) => [...c.matchAll(/(?<![\w-])tt-[a-z0-9]+(?:-[a-z0-9]+)*(?![\w-])/g)].map((m) => m[0])),
  )
  const errors = []
  for (const path of stylesheets(src)) {
    const file = relative(web, path).split('\\').join('/')
    const reported = new Set()
    for (const { selector, line } of allRules(readFileSync(path, 'utf8'))) {
      for (const cls of classSelectors(selector)) {
        if (!cls.startsWith('tt-') || used.has(cls) || built.has(cls) || reported.has(cls)) continue
        reported.add(cls)
        errors.push(
          `${file}:${line}: .${cls} has a rule but nothing in the app uses the class: delete its rules (or, if a module builds the name from pieces, add it to BUILT_FROM_PIECES with where)`,
        )
      }
    }
  }
  for (const cls of built.keys()) {
    if (!used.has(cls) && ![...stylesheets(src)].some((p) => readFileSync(p, 'utf8').includes(`.${cls}`))) {
      errors.push(`${cls} is in BUILT_FROM_PIECES but no rule names it: remove it from the list`)
    }
  }
  return errors
}

export const ALIASES = 'src/ui/legacy.css'

export function unusedAliasErrors(web) {
  const path = join(web, ALIASES)
  if (!existsSync(path)) return []
  const defined = allDeclarations(readFileSync(path, 'utf8'))
  const name = /--tt-[a-z0-9]+(?:-[a-z0-9]+)*(?![\w-])/g
  const read = new Set()
  for (const p of stylesheets(join(web, 'src'))) {
    if (relative(web, p).split('\\').join('/') === ALIASES) continue
    const css = blankCssComments(readFileSync(p, 'utf8'))
    for (const m of css.matchAll(/var\(\s*(--tt-[a-z0-9]+(?:-[a-z0-9]+)*)(?![\w-])/g)) read.add(m[1])
  }
  for (const code of usersCode(web)) for (const m of code.matchAll(name)) read.add(m[0])
  return defined
    .filter(({ name }) => !read.has(name))
    .map(({ name, line }) => `${ALIASES}:${line}: ${name} is defined but nothing reads it: delete it`)
}

function allDeclarations(css) {
  const text = blankCssComments(css)
  const out = []
  for (const m of text.matchAll(/(?<![\w-])(--tt-[a-z0-9-]+)\s*:/g)) {
    out.push({ name: m[1], line: text.slice(0, m.index).split('\n').length })
  }
  return out
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const at = process.argv.indexOf('--root')
  const web = at > 0 ? process.argv[at + 1] : join(dirname(fileURLToPath(import.meta.url)), '..')
  const errors = [...check(web), ...(at > 0 ? [] : [...unusedClassErrors(web), ...unusedAliasErrors(web)])]
  if (errors.length > 0) {
    console.error(`css-check: ${errors.length} problem${errors.length === 1 ? '' : 's'}\n`)
    for (const e of errors) console.error(`  ${e}`)
    process.exit(1)
  }
  console.log(
    'css-check: no selector is declared twice, every tt- and ui- class a component uses has a rule and every tt- class a rule names is used, no colour is written outside the themes, no old rule reaches into a component, and src/ui/ takes its sizes, spaces, radii, shadows and durations from the tokens',
  )
}
