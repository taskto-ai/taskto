#!/usr/bin/env node
// Checks the web app's stylesheets (every .css file under src/: the design system's files in src/ui/,
// the screens' beside their components, and styles.css, the list that imports those) against each
// other and against the components. CI runs it; so can anyone:
// `pnpm --filter @taskto/web run css`.
//
// Fails on:
// - a `.tt-` or `.ui-` selector declared more than once at the top level, in one file or across files (a
//   second rule with the same selector silently overrides or merges with the first; a state or a
//   screen size goes in an at-rule block, or in a selector of its own). A rule inside `@layer`
//   blocks only is at the top level: a layer orders rules, it doesn't set them apart;
// - a `tt-` or `ui-` class a component's className uses that no rule of any stylesheet mentions;
// - the reverse for the old classes: a rule naming a `tt-` class that no module mentions (a dead rule,
//   left behind when a screen moved onto the components; see unusedClassErrors);
// - a colour literal (a hex colour, rgb(), hsl() and the other colour functions, or a named colour
//   such as `white`, or a system colour such as `Canvas`) in a declaration outside src/ui/themes/: a colour is a role a theme sets, used
//   through var(--color-…) (docs/design.md). Comments, strings and url() are not looked in;
// - under src/ui/, apart from the files that define the tokens (tokens.css, themes/, and the old
//   names' aliases in legacy.css): a font size, a spacing value (margin, padding, gap), a radius, a
//   shadow or a duration written as a literal, not as a token (var(--space-…), var(--text-…) …).
//   A few literals are not on any scale and are allowed: SCALE_LITERALS, each with its reason. The
//   screens' older stylesheets are not held to this until they are rebuilt.
// - a rule in a stylesheet outside src/ui/, or in src/ui/base.css, that can match an element
//   wherever it is (by its name, a state, a pseudo-element, an attribute, `*`; not `.tt-x h2`) and
//   doesn't leave the design system's elements out with OPT_OUT, unless EVERYWHERE lists its
//   selector and every property it sets: such a rule would beat the components' own styles (in no
//   layer whatever their specificity; in base.css only if it left its layer);
// - how the app loads its stylesheets, when the root has the app's entry (src/main.tsx): its first
//   import is ui/index.css, which declares the layers' order before any stylesheet uses one; every
//   .css under src/ is loaded exactly one way (listed in src/styles.css, imported by
//   src/ui/index.css, or imported by a module); src/styles.css is nothing but OLD_STYLESHEETS, in
//   that order, each a plain @import; those files declare no layer, apart from ui/base.css, which
//   is one @layer base block, then in no layer only rules EVERYWHERE allows, and last the touch
//   screen's @media (pointer: coarse) block, its rules opting out (docs/design.md, "Where styles
//   live").
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
//   in the same file (a map imported from another file is read there), and the strings of an
//   array a constant of any name holds (`const cls = ['ui-a', big ? 'ui-a-big' : '']`, the way the
//   components build their classes). A string that is only a class name in another kind of
//   constant (`const cls = 'ui-a'`), or built from pieces (`'tt-' + kind`), is not seen: such a
//   class has to be written out in the stylesheet's component anyway, and the check can't tell it
//   is missing.
//
// --root <dir> checks another web directory (its src/ and the stylesheets in it), for the script's
// tests.

import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

// Classes that components use as hooks (for tests, for scripts, or to say what an element is)
// and that deliberately have no rule. Each needs a reason; a class that is merely not styled yet
// gets a rule, not an entry here.
export const UNSTYLED = new Map([
  [
    'ui-control',
    "marks a control's native element (Button, Input, Checkbox …); like every ui- class it keeps the old element rules off, and no rule needs it on its own",
  ],
])

// The app's own classes: `tt-` (the screens) and `ui-` (the design system's components, src/ui/).
const CLASS = /(?:tt|ui)-[a-z0-9]+(?:-[a-z0-9]+)*/g
// A class selector: the name after a dot.
const CLASS_SELECTOR = /\.((?:tt|ui)-[a-z0-9]+(?:-[a-z0-9]+)*)(?![\w-])/g

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
    'yellowgreen ' +
    // The system colours (CSS Color 4, and the deprecated ones browsers still take): the
    // browser's or the OS's colours, which no theme sets, so they are literals here too.
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

// What is inside the parentheses of a value's functions; the rest is blanked. The fallback of a
// var() at the value's top level is blanked too: it stands where an identifier would. A var()
// inside another function (a gradient's colour stop) keeps its fallback looked in.
function functionArguments(value) {
  const stack = [] // for each open parenthesis: is it a var()'s?
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
  // A table in capitals (an object or an array), or an array a component builds its classes from,
  // whatever its name, typed or not: `const cls: string[] = ['ui-button', big ? 'ui-button-big' : '']`.
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

// --- How the app loads its stylesheets -------------------------------------------------------

// The rules of the one stylesheet the app had before its design system, now in files of their own,
// as src/styles.css lists them: the order is the cascade (most are in no layer, where the later of
// two equal rules wins), so it is pinned here. A screen's new stylesheet is not one of these: its
// component imports it. Paths are under src/.
export const OLD_STYLESHEETS = [
  'ui/base.css',
  'legacy-shared.css',
  'auth/auth.legacy.css',
  'boards/boards.legacy.css',
  'markdown.legacy.css',
  'settings/settings.legacy.css',
  'pwa/pwa.legacy.css',
  'offline/offline.legacy.css',
]
const LIST = 'src/styles.css'
// The cascade layers, in order, as ui/index.css and index.html declare them.
export const LAYERS = ['tokens', 'themes', 'base', 'components', 'screens']
const ENTRY = 'src/main.tsx'
const DESIGN_ENTRY = 'src/ui/index.css'
const BASE = 'src/ui/base.css'

// A stylesheet's top-level statements: `@import`s with their url and what follows it, and blocks
// with their prelude, comments left out.
// The stylesheet with its comments blanked (newlines kept, so offsets and lines stay); a `/*`
// inside a string is not a comment.
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
      // To the block's end: braces inside a string (`content: "}"`) or escaped are not the block's.
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

// The .css files a module imports for their styles (`import './x.css'`), resolved under the root:
// not `?raw` or `?url`, which load no style.
// The modules the app reaches from its entry: static imports, re-exports and dynamic import(),
// by relative path (with or without the extension, or a folder's index).
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
    // import.meta.glob('./*.gallery.tsx'): every file in that folder its pattern matches.
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
  const out = new Map() // file → the modules that import it
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

// The files a stylesheet @imports, resolved under the root.
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

// The old stylesheets as src/styles.css lists them, read, in order: what the tests of the old
// rules read (the list itself holds no rule). Missing files are left out.
export function oldStylesheets(web) {
  return cssImports(web, LIST)
    .filter((s) => existsSync(join(web, s.file)))
    .map((s) => ({ file: s.file, css: readFileSync(join(web, s.file), 'utf8') }))
}

function loadingErrors(web, sheets) {
  const errors = []
  // The entry's first import: the layers are declared by the first stylesheet the build emits.
  const entry = blankComments(readFileSync(join(web, ENTRY), 'utf8'))
  const entryImports = [...entry.matchAll(/\bimport\s+(?:[^'"]*?\bfrom\s+)?["']([^"']+)["']/g)].map((m) => m[1])
  const first = entryImports.length > 0 ? [null, entryImports[0]] : null
  if (first?.[1] !== './ui/index.css') {
    errors.push(
      `${ENTRY}: its first import is ${first ? `'${first[1]}'` : 'missing'}, not './ui/index.css': the build emits CSS in import order, so a component's @layer block would come before the layers' order is declared, and @layer base would beat @layer components`,
    )
  }

  // ui/index.css: the layers' order first, then its files, each imported with no condition.
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

  // The list: plain @imports of OLD_STYLESHEETS, in order, and nothing else.
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

  // Every stylesheet is loaded, and one way only.
  // Each way a stylesheet is loaded, by kind: listed in the list, imported by ui/index.css, or
  // imported by modules (several modules importing one component's stylesheet are one way).
  const ways = new Map(sheets.map((f) => [f, []]))
  const add = (f, kind, way) => ways.get(f)?.push({ kind, way })
  // The list itself counts as loaded only if the entry imports it.
  if (!entryImports.includes('./styles.css')) {
    errors.push(`${ENTRY}: doesn't import './styles.css', so no old stylesheet it lists is loaded`)
  }
  for (const f of listed) add(f, 'list', `listed in ${LIST}`)
  for (const s of cssImports(web, DESIGN_ENTRY)) add(s.file, 'index', `imported by ${DESIGN_ENTRY}`)
  for (const [f, modules] of moduleImports(web)) {
    // The entry's own imports are of the list and of ui/index.css, which load in ways of their own.
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

  // The old stylesheets declare no layer, but base.css, which is one @layer base block and then
  // one block in no layer.
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

// ui/base.css is three parts: one @layer base block; then, in no layer, only rules that reach every
// element on purpose (EVERYWHERE, with only the properties it lists), which must keep winning over a
// component's own rule; and last the touch screen's @media (pointer: coarse) block, in no layer,
// every rule of it leaving the components' elements out.
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

// Where every size, space, radius, shadow and duration is a token: the design system's files,
// except those that define the tokens.
const TOKENS_ONLY = 'src/ui/'
const DEFINE_TOKENS = ['src/ui/tokens.css', 'src/ui/legacy.css', THEMES]

// Literals that are not on a scale and never will be, so they may be written as they are.
export const SCALE_LITERALS = new Map([
  ['0', 'nothing: no space, no radius, no time (with or without a unit)'],
  ['1px', 'a hairline: a border-wide offset, finer than the smallest step'],
  ['-1px', 'a hairline the other way: pulls a box back by a border, as the visually hidden pattern does'],
  ['100%', 'the whole of the box'],
  ['50%', 'half of the box: the radius of a circle'],
])

// The properties whose lengths and times are a scale's: a font size, a space, a radius, a shadow
// and a duration.
const SCALE_PROPERTY =
  /^(font|font-size|margin(-.+)?|padding(-.+)?|scroll-margin(-.+)?|scroll-padding(-.+)?|gap|row-gap|column-gap|grid-gap|grid-row-gap|grid-column-gap|border(-.+)?-radius|(-webkit-)?box-shadow|text-shadow|(-webkit-)?transition(-duration|-delay)?|(-webkit-)?animation(-duration|-delay)?)$/i

// Custom properties of the design system's own files that may hold a literal size, each with its
// reason: what such a property holds is used through var() where the check can't see it, so a
// literal there is held to the same rule as a declaration's.
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

// The first literal length or time a declaration's value holds where a token belongs, if any.
// A var() without a fallback is a token; a fallback is looked in. env() (the safe areas) is the
// device's, but its fallback is looked in too; a unitless number (a line height, a weight, a factor
// in calc()) is no length.
export function scaleLiteral(value, property) {
  // A registered property's initial value (@property) is a custom property's value.
  if (property.toLowerCase() === 'initial-value') property = '--registered'
  if (!property.startsWith('--') && !SCALE_PROPERTY.test(property)) return undefined
  if (property.startsWith('--') && LOCAL_LITERALS.has(property)) return undefined
  let bare = value
    .replace(/url\((?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^)]*)\)/gi, 'url()')
    .replace(/"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'/g, '""')
  // var(--a) and env(name) go; a fallback, var(--a, x) or env(name, x), stays, innermost first.
  for (let prev = ''; prev !== bare; ) {
    prev = bare
    bare = bare.replace(
      /(?<![\w-])(?:var\(\s*--[\w-]+|env\(\s*[\w-]+(?:\s+\d+)*)\s*(?:,([^()]*))?\)/gi,
      (_, fallback) => ` ${fallback ?? ''} `,
    )
  }
  for (const m of bare.matchAll(/(?<![\w.#-])-?(?:\d+\.?\d*|\.\d+)(?:e-?\d+)?([a-z%]+)(?![\w-])/gi)) {
    const literal = m[0].toLowerCase()
    if (/^-?0*\.?0*[a-z%]+$/.test(literal)) continue // a zero, whatever its unit
    // A custom property may hold a share of something (a slide's distance, a width): its sizes
    // and times are what a token would be, not its percentages.
    if (property.startsWith('--') && literal.endsWith('%')) continue
    if (SCALE_LITERALS.has(literal)) continue
    return m[0]
  }
  return undefined
}

// --- Rules that reach every element --------------------------------------------------------
//
// The old stylesheets (every one outside src/ui/) are in no cascade layer, so a rule of theirs
// that can match an element wherever it is beats every component of the design system, whatever
// the specificity of the component's own rule. Such a rule styles by an element's name (`h2`,
// `button`), by a state or a pseudo-element alone (`:focus-visible`, `::placeholder`), by an
// attribute alone, by `*`, or by `:is(…)`; and nothing scopes it but what wraps every page
// (WRAPPERS). Each one leaves the design system's elements out: every element a component
// draws has a class that starts with `ui-`, and the rule's subject carries OPT_OUT, which adds no
// specificity (docs/design.md, "The components"). A rule scoped by an old container
// (`.tt-markdown h2`) reaches only inside it, and is left alone.

export const OPT_OUT = ':not(:where([class^="ui-"], [class*=" ui-"]))'

// What wraps every page: a rule scoped by one of these still reaches every component.
export const WRAPPERS = new Set(['#root', '.tt-shell', '.tt-body', '.tt-page', '#tt-main'])

// Rules that deliberately reach every element, the design system's too: the selector, the only
// properties it may set, and the reason. A rule that sets anything else is not allowed by it.
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

// Every rule's selectors at any depth (inside @media and @layer too, not inside @keyframes,
// whose `from` and `to` are not elements), each with its line and the properties its block sets.
export function allRules(css) {
  const out = []
  const blocks = [] // for each open block: { kind: 'keyframes' | 'at' | 'rule', rules?: [...] }
  let prelude = ''
  let preludeLine = 1
  let line = 1
  const declare = (text) => {
    const top = blocks.at(-1)
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
          for (const selector of splitSelectors(text)) {
            const r = { selector, line: preludeLine, properties: [] }
            rules.push(r)
            out.push(r)
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

// A selector's compounds: "a > b:has(> c) d" → ["a", "b:has(> c)", "d"].
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

// A compound without what is inside its parentheses and brackets: what it says of the element
// itself (`:is(.a, h2)` says nothing of its own, so it is not scoped by `.a`).
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

// The classes and ids a compound names of its own element.
const scopes = (compound) => own(compound).match(/[.#][\w-]+/g) ?? []

// Whether a rule can match an element wherever it is in the app: its subject names no class or
// id of its own, and every compound before it is unscoped or a wrapper of every page.
export function reachesAnywhere(selector) {
  const parts = compounds(selector)
  const subject = parts.at(-1) ?? ''
  if (scopes(subject).length > 0) return false
  return parts.slice(0, -1).every((p) => scopes(p).every((s) => WRAPPERS.has(s)))
}

// Whether a compound leaves the design system's elements out: OPT_OUT is one of its own
// pseudo-classes, at the top level, not inside another function (`:not(:not(…OPT_OUT…))` would
// select exactly those elements).
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

// Whether a rule that reaches anywhere is allowed: it leaves the design system's elements out,
// or EVERYWHERE lists its selector and it sets only the properties listed there.
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
  const seen = new Map() // selector → { file, line }
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
    // Under src/ui/, every declaration; in the files that define the tokens, a custom property is
    // the definition of one and may hold a literal, but a rule's own declarations may not.
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

// --- Old rules nothing uses ------------------------------------------------------------------

// A rule that names a `tt-` class (the old stylesheet's) that no module of the app mentions can
// match nothing: it was left behind when its screen moved onto the components, and it goes. A
// module mentions a class when the name is anywhere in its code (a className, a lookup table, a
// string a class is built from), its comments left out; tests and the gallery's specimens don't
// count, nor does a name that is only the start of a longer one. Only the old classes are held to
// it: a component's `ui-` class can be built from pieces (`ui-button-${variant}`), which a search
// for the name doesn't see.
export function unusedClassErrors(web) {
  const src = join(web, 'src')
  const code = components(src)
    .filter((f) => !/\.gallery\.tsx?$/.test(f))
    .map((f) => blankComments(readFileSync(f, 'utf8')))
  // The page's own markup, its comments left out as a module's are.
  const html = join(web, 'index.html')
  if (existsSync(html)) code.push(readFileSync(html, 'utf8').replace(/<!--[\s\S]*?-->/g, ''))
  const used = new Set(
    code.flatMap((c) => [...c.matchAll(/(?<![\w-])tt-[a-z0-9]+(?:-[a-z0-9]+)*(?![\w-])/g)].map((m) => m[0])),
  )
  const errors = []
  for (const path of stylesheets(src)) {
    const file = relative(web, path).split('\\').join('/')
    // The rules' selectors only: not a declaration's value (`content: ".tt-x"`), an attribute
    // selector's value or a comment.
    const reported = new Set()
    for (const { selector, line } of allRules(readFileSync(path, 'utf8'))) {
      for (const cls of classSelectors(selector)) {
        if (!cls.startsWith('tt-') || used.has(cls) || reported.has(cls)) continue
        reported.add(cls)
        errors.push(`${file}:${line}: .${cls} has a rule but nothing in the app uses the class: delete its rules`)
      }
    }
  }
  return errors
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const at = process.argv.indexOf('--root')
  const web = at > 0 ? process.argv[at + 1] : join(dirname(fileURLToPath(import.meta.url)), '..')
  // The app's own tree (not a test's): its old rules nothing uses any more, too.
  const errors = [...check(web), ...(at > 0 ? [] : unusedClassErrors(web))]
  if (errors.length > 0) {
    console.error(`css-check: ${errors.length} problem${errors.length === 1 ? '' : 's'}\n`)
    for (const e of errors) console.error(`  ${e}`)
    process.exit(1)
  }
  console.log(
    'css-check: no selector is declared twice, every tt- and ui- class a component uses has a rule and every tt- class a rule names is used, no colour is written outside the themes, no old rule reaches into a component, and src/ui/ takes its sizes, spaces, radii, shadows and durations from the tokens',
  )
}
