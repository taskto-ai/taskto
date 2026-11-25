#!/usr/bin/env node
// Checks the web app's stylesheets (every .css file under src/: styles.css, and the design system's
// files in src/ui/) against each other and against the components. CI runs it; so can anyone:
// `pnpm --filter @taskto/web run css`.
//
// Fails on:
// - a `.tt-` selector declared more than once at the top level, in one file or across files (a
//   second rule with the same selector silently overrides or merges with the first; a state or a
//   screen size goes in an at-rule block, or in a selector of its own). A rule inside `@layer`
//   blocks only is at the top level: a layer orders rules, it doesn't set them apart;
// - a `tt-` class a component's className uses that no rule of any stylesheet mentions;
// - a colour literal (a hex colour, rgb(), hsl() and the other colour functions, or a named colour
//   such as `white`) in a declaration outside src/ui/themes/: a colour is a role a theme sets, used
//   through var(--color-…) (docs/design.md). Comments, strings and url() are not looked in.
//
// Dependency-free: the stylesheet is read by a small scanner (comments and strings skipped), the
// components by looking at what each className holds.
//
// What it sees, and what it doesn't:
// - In the stylesheet, a class is a `.tt-name` selector. An attribute selector's value
//   (`[data-state="tt-name"]`), a string, a comment and an `@keyframes` name are not classes.
// - In a component, a class is a `tt-` name inside a className: a string, or an expression (a
//   template, a conditional, a join), and the strings of a lookup table that a constant holds:
//   `const NAME = { ok: 'tt-ok' }` or `const NAME = ['tt-a', 'tt-b']`, with the name in capitals,
//   in the same file (a map imported from another file is read there). A string that is only a
//   class name, in a constant whose name is not in capitals, or in a function's body, or built
//   from pieces (`'tt-' + kind`), is not seen: such a class has to be written out in the
//   stylesheet's component anyway, and the check can't tell it is missing.
//
// --root <dir> checks another web directory (its src/ and the stylesheets in it), for the script's
// tests.

import { readdirSync, readFileSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

// Classes that components use as hooks (for tests, for scripts, or to say what an element is)
// and that deliberately have no rule. Each needs a reason; a class that is merely not styled yet
// gets a rule, not an entry here.
export const UNSTYLED = new Map([
  // (none today)
])

const CLASS = /tt-[a-z0-9]+(?:-[a-z0-9]+)*/g
// A class selector: the name after a dot.
const CLASS_SELECTOR = /\.(tt-[a-z0-9]+(?:-[a-z0-9]+)*)(?![\w-])/g

// The class selectors of a rule's prelude, without what is inside an attribute selector's
// brackets or a string.
export function classSelectors(prelude) {
  const bare = prelude.replace(/\[[^\]]*\]/g, '').replace(/"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'/g, '""')
  return [...bare.matchAll(CLASS_SELECTOR)].map((m) => m[1])
}

// The stylesheet's rules at the top level (at depth 0, or inside `@layer` blocks only): each with
// its selector list and the classes it names. Other at-rules (@media, @keyframes, @font-face …) are
// not rules here, but the classes inside an at-rule's block still count as styled. And every
// declaration, with its line, for the colour check.
export function scan(css) {
  const top = [] // selectors declared at the top level, each with its line
  const mentioned = new Set()
  const declarations = [] // { property, value, line }
  const blocks = [] // for each open block: 'layer', 'at' (another at-rule) or 'rule'
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

  // A declaration is `property: value` in any block: a rule's, or an at-rule's that holds
  // declarations (@property, @font-face, @page). A selector ends at `{`, so it never gets here.
  function declaration(text, at) {
    if (blocks.length === 0) return
    const colon = text.indexOf(':')
    if (colon < 0) return
    const property = text.slice(0, colon).trim()
    if (!/^-{0,2}[a-zA-Z][\w-]*$/.test(property)) return
    declarations.push({ property, value: text.slice(colon + 1).trim(), line: at })
  }
}

// The colours CSS knows by name (CSS Color 4), and the colour functions.
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
    'yellowgreen'
  ).split(' '),
)
const COLOR_FUNCTION = /(?<![\w-])(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\(/i
const HEX = /#[0-9a-fA-F]{3,8}(?![\w-])/

// Properties whose words are names the author picks (an animation, a font, a grid area …), where
// `red` is an identifier and not a colour. Hex colours and colour functions are still looked for.
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

// What is inside the parentheses of a value's functions; the rest is blanked. A var()'s fallback
// is blanked too: it stands where the var() is, so it is an identifier there as well.
function functionArguments(value) {
  const stack = [] // for each open parenthesis: is it a var()'s?
  let out = ''
  for (let i = 0; i < value.length; i++) {
    const c = value[i]
    if (c === ')') stack.pop()
    const inside = stack.length > 0 && !stack.includes(true)
    out += inside && c !== '(' ? c : ' '
    if (c === '(') stack.push(/(?<![\w-])var$/i.test(value.slice(0, i)))
  }
  return out
}

// The colour literal a declaration's value holds, if any: not in a string, a url() or a custom
// property's name, and a colour's name not where a name is an identifier.
export function colorLiteral(value, property = '') {
  const bare = value
    .replace(/url\((?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^)]*)\)/gi, 'url()')
    .replace(/"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'/g, '""')
    .replace(/--[\w-]+/g, '--x')
  const hex = HEX.exec(bare)
  if (hex) return hex[0]
  const fn = COLOR_FUNCTION.exec(bare)
  if (fn) return `${fn[0]})`
  // Where words are names the author picks, only a function's arguments can hold a colour's name
  // (`list-style: linear-gradient(red, blue)`); a bare `red` there is an identifier.
  const words = IDENTIFIER_PROPERTIES.has(property.toLowerCase()) ? functionArguments(bare) : bare
  for (const word of words.matchAll(/(?<![\w-])[a-zA-Z]+(?![\w-])/g)) {
    if (NAMED_COLORS.has(word[0].toLowerCase())) return word[0]
  }
  return undefined
}

// "a, b > c" → ["a", "b > c"], not splitting inside parentheses (:is(a, b)).
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

// The `tt-` classes inside what a component passes as className: a string, or an expression
// (a template, a conditional, a join) read up to its closing brace.
export function classesUsed(raw) {
  const source = blankComments(raw)
  const used = new Map() // class → offset of the first use
  for (const m of source.matchAll(/\bclassName\s*=\s*/g)) {
    const start = m.index + m[0].length
    const end = expressionEnd(source, start)
    for (const c of source.slice(start, end).matchAll(CLASS)) {
      // A whole class name only: not `--tt-fg` (a custom property), nor the end of `not-tt-x`.
      if (/[\w-]/.test(source[start + c.index - 1] ?? '')) continue
      if (!used.has(c[0])) used.set(c[0], start + c.index)
    }
  }
  tableClasses(source, used)
  return used
}

// The strings of the lookup tables a component declares (`const NAME = { ok: 'tt-ok' }`,
// `const NAME = ['tt-a']`; a name in capitals) that are class names only.
// The source with its comments replaced by spaces (newlines kept, so offsets and lines stay):
// `// className="tt-old"` and `{/* className="tt-old" */}` use nothing. Strings and templates are
// skipped whole, so a `//` in a URL is not a comment.
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
  for (const m of source.matchAll(/\bconst\s+[A-Z][A-Z0-9_]*\b[^=\n]*=\s*(?=[{[])/g)) {
    const start = m.index + m[0].length
    const end = tableEnd(source, start)
    for (const lit of source
      .slice(start, end)
      .matchAll(/(["'])(tt-[a-z0-9]+(?:-[a-z0-9]+)*(?: tt-[a-z0-9]+(?:-[a-z0-9]+)*)*)\1/g)) {
      for (const c of lit[2].matchAll(CLASS)) {
        const at = start + lit.index + 1 + c.index
        if (!used.has(c[0])) used.set(c[0], at)
      }
    }
  }
}

// The end of the {…} or […] that begins at start.
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

// A template literal, whose ${ } holes hold code (and may hold templates).
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

// Where the colour literals live: the brand themes, and nowhere else.
const THEMES = 'src/ui/themes/'

export function check(web) {
  const errors = []
  const mentioned = new Set()
  const seen = new Map() // selector → { file, line }
  for (const path of stylesheets(join(web, 'src'))) {
    const file = relative(web, path).split('\\').join('/')
    const sheet = scan(readFileSync(path, 'utf8'))
    for (const name of sheet.mentioned) mentioned.add(name)
    for (const { selector, line } of sheet.top) {
      if (!/\.tt-/.test(selector)) continue
      const first = seen.get(selector)
      if (first) {
        const where = first.file === file ? `line ${first.line}` : `${first.file}:${first.line}`
        errors.push(
          `${file}:${line}: ${selector} is declared again (first at ${where}): merge the two, or give one its own class`,
        )
      } else seen.set(selector, { file, line })
    }
    if (file.startsWith(THEMES)) continue
    for (const { property, value, line } of sheet.declarations) {
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

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const at = process.argv.indexOf('--root')
  const web = at > 0 ? process.argv[at + 1] : join(dirname(fileURLToPath(import.meta.url)), '..')
  const errors = check(web)
  if (errors.length > 0) {
    console.error(`css-check: ${errors.length} problem${errors.length === 1 ? '' : 's'}\n`)
    for (const e of errors) console.error(`  ${e}`)
    process.exit(1)
  }
  console.log(
    'css-check: no selector is declared twice, every tt- class a component uses has a rule, and no colour is written outside the themes',
  )
}
