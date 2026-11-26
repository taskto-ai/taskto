import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { afterEach, describe, expect, test } from 'vitest'
import { allowedEverywhere, allRules, OLD_STYLESHEETS, OPT_OUT, reachesAnywhere } from '../scripts/css-check.mjs'

// scripts/css-check.mjs: the stylesheets are global (one list of files, every rule in force on every
// page), so a selector declared twice, in one file or two, a class a component uses that no rule
// has, a colour outside the themes and a literal size in the design system are caught here, not on
// a screen.
const web = resolve(__dirname, '..')
const script = join(web, 'scripts/css-check.mjs')

const dirs: string[] = []
afterEach(() => {
  for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true })
})

// A web directory with this stylesheet and these components, and what the script says of it.
function run(css: string, components: Record<string, string> = {}, args: string[] = []) {
  const root = mkdtempSync(join(tmpdir(), 'css-check-'))
  dirs.push(root)
  mkdirSync(join(root, 'src'))
  writeFileSync(join(root, 'src/styles.css'), css)
  for (const [name, source] of Object.entries(components)) writeFileSync(join(root, 'src', name), source)
  const r = spawnSync(process.execPath, [script, '--root', root, ...args], { encoding: 'utf8' })
  return { code: r.status, out: r.stdout, err: r.stderr }
}

const component = (cls: string) => `export const A = () => <p className=${cls}>x</p>\n`

test('passes when every selector is declared once and every class used has a rule', () => {
  const r = run(
    `/* a comment mentioning .tt-card { } is not a rule */
.tt-card { color: inherit; }
.tt-card h2 { margin: 0; }
@media (max-width: 48rem) { .tt-card { padding: 0; } }
@keyframes spin { from { opacity: 0; } to { opacity: 1; } }
`,
    { 'A.tsx': component('"tt-card"') },
  )
  expect(r.err).toBe('')
  expect(r.code).toBe(0)
})

test('fails when a selector is declared twice at the top level, alone or in a group', () => {
  const twice = run('.tt-snippet { color: inherit; }\n.tt-other { color: inherit; }\n.tt-snippet { display: grid; }\n')
  expect(twice.code).toBe(1)
  expect(twice.err).toContain('src/styles.css:3: .tt-snippet is declared again (first at line 1)')

  const grouped = run('.tt-a, .tt-b { color: inherit; }\n.tt-b { margin: 0; }\n')
  expect(grouped.code).toBe(1)
  expect(grouped.err).toContain('.tt-b is declared again')
  expect(grouped.err).not.toContain('.tt-a is declared again')
})

test('a selector in an at-rule block, or with another selector in front of it, is not the same one', () => {
  const r = run(
    '.tt-a { color: inherit; }\n.tt-a:hover { color: currentColor; }\n.tt-x .tt-a { margin: 0; }\n@media print { .tt-a { color: initial; } }\n',
  )
  expect(r.err).toBe('')
  expect(r.code).toBe(0)
})

test('fails on a tt- class a component uses that has no rule, in a string, a template or a conditional', () => {
  const css = '.tt-known { color: inherit; }\n'
  for (const [expr, missing] of [
    ['"tt-known tt-missing"', 'tt-missing'],
    // biome-ignore lint/suspicious/noTemplateCurlyInString: the component's source, not this file's
    ['{`tt-known ${on ? "tt-on" : ""}`}', 'tt-on'],
    ['{on ? "tt-known" : "tt-off"}', 'tt-off'],
    ["{['tt-known', 'tt-listed'].join(' ')}", 'tt-listed'],
  ]) {
    const r = run(css, { 'A.tsx': component(expr) })
    expect(r.code, expr).toBe(1)
    expect(r.err, expr).toContain(`src/A.tsx:1: ${missing} is used but no rule`)
  }
})

test('only className counts: ids, test files and custom properties are not classes', () => {
  const r = run('.tt-known { color: inherit; }\n', {
    'A.tsx': `export const A = () => <p id="tt-anchor" style={{ color: 'var(--tt-fg)' }} className="tt-known">x</p>\n`,
    'A.test.tsx': 'export const B = () => <p className="tt-only-in-a-test">x</p>\n',
  })
  expect(r.err).toBe('')
  expect(r.code).toBe(0)
})

test("the app's own stylesheet and components pass", () => {
  const r = spawnSync(process.execPath, [script], { encoding: 'utf8', cwd: web })
  expect(r.stderr).toBe('')
  expect(r.status).toBe(0)
})

test('a class a lookup table holds counts as used; an attribute selector is not a rule for a class', () => {
  const css =
    '.tt-known { color: inherit; }\n[data-state="tt-missing"] { color: currentColor; }\n.tt-other[data-x=\'tt-also\'] { color: inherit; }\n'
  const table = `const HEALTH: Record<string, string | undefined> = { none: 'tt-known', bad: 'tt-missing' }
export const A = ({ h }: { h: string }) => <p className={HEALTH[h]}>x</p>
`
  const r = run(css, { 'A.tsx': table })
  expect(r.code).toBe(1)
  expect(r.err).toContain('src/A.tsx:1: tt-missing is used but no rule')
  expect(r.err).not.toContain('tt-known is used')

  // An id in the same file, and a table not in capitals, are not classes it reads.
  const quiet = run('.tt-known { color: inherit; }\n', {
    'A.tsx': `const lower = { a: 'tt-nothing' }\nexport const A = () => <p id="tt-anchor" className="tt-known">{lower.a}</p>\n`,
  })
  expect(quiet.err).toBe('')
  expect(quiet.code).toBe(0)
})

test('a class is a whole name: `.not-tt-x` is no rule for tt-x, and `not-tt-x` is no use of it', () => {
  const r = run('.not-tt-missing { color: inherit; }\n.tt-known { color: inherit; }\n', {
    'A.tsx': component('"tt-known tt-missing"'),
  })
  expect(r.code).toBe(1)
  expect(r.err).toContain('tt-missing is used but no rule')

  const quiet = run('.tt-known { color: inherit; }\n', { 'A.tsx': component('"tt-known not-tt-else"') })
  expect(quiet.err).toBe('')
  expect(quiet.code).toBe(0)
})

test('a className in a comment uses nothing, and a // in a string is not a comment', () => {
  const r = run('.tt-known { color: inherit; }\n', {
    'A.tsx': [
      '// <p className="tt-old">x</p>',
      '/* className="tt-older" */',
      'export const A = () => (',
      '  <p className="tt-known">',
      '    {/* className="tt-oldest" */}',
      '    <a href="https://example.test/x" className="tt-after-url">x</a>',
      '  </p>',
      ')',
      '',
    ].join('\n'),
  })
  expect(r.code).toBe(1)
  expect(r.err).not.toMatch(/tt-old\b|tt-older|tt-oldest/)
  // The class after a URL's // is still read, and reported at its own line.
  expect(r.err).toContain('src/A.tsx:6: tt-after-url is used but no rule')
})

// More stylesheets than styles.css: the design system's files under src/ui/.
function runFiles(files: Record<string, string>) {
  const root = mkdtempSync(join(tmpdir(), 'css-check-'))
  dirs.push(root)
  for (const [name, css] of Object.entries({ 'styles.css': '', ...files })) {
    mkdirSync(join(root, 'src', name, '..'), { recursive: true })
    writeFileSync(join(root, 'src', name), css)
  }
  const r = spawnSync(process.execPath, [script, '--root', root], { encoding: 'utf8' })
  return { code: r.status, out: r.stdout, err: r.stderr }
}

test('a selector declared again in another stylesheet, or inside @layer blocks, is declared twice', () => {
  const across = runFiles({ 'styles.css': '.tt-a { color: inherit; }\n', 'ui/button.css': '\n.tt-a { margin: 0; }\n' })
  expect(across.code).toBe(1)
  expect(across.err).toContain('src/ui/button.css:2: .tt-a is declared again (first at src/styles.css:1)')

  const layered = runFiles({
    'ui/button.css': '@layer components {\n  .tt-b { margin: 0; }\n}\n@layer screens { .tt-b { padding: 0; } }\n',
  })
  expect(layered.code).toBe(1)
  expect(layered.err).toContain('src/ui/button.css:4: .tt-b is declared again (first at line 2)')

  const media = runFiles({
    'ui/button.css': '@layer components {\n.tt-c { margin: 0; }\n@media print { .tt-c { margin: 1px; } }\n}\n',
  })
  expect(media.err).toBe('')
  expect(media.code).toBe(0)
})

test('a colour is written only in a theme: a literal anywhere else fails', () => {
  for (const [value, literal] of [
    ['#fff', '#fff'],
    ['1px solid #11111080', '#11111080'],
    ['rgba(0, 0, 0, 0.5)', 'rgba()'],
    ['0 2px 4px oklch(0.5 0.1 200)', 'oklch()'],
    ['white', 'white'],
    ['light-dark(var(--color-bg), Black)', 'Black'],
  ]) {
    const r = runFiles({ 'ui/box.css': `.tt-box {\n  background: ${value};\n}\n` })
    expect(r.code, value).toBe(1)
    expect(r.err, value).toContain(`src/ui/box.css:2: background has the colour literal ${literal}`)
  }
  const prop = runFiles({ 'ui/tokens.css': ':root { --space-1: 4px; --color-bg: #faf8f4; }\n' })
  expect(prop.err).toContain('--color-bg has the colour literal #faf8f4')
})

test('a theme may write colours, and comments, strings, urls, names and ids are not colours', () => {
  const r = runFiles({
    'ui/themes/brand.css':
      '@layer themes { :root { --color-bg: light-dark(#faf8f4, #111110); --color-paper: white; } }\n',
    'styles.css': `/* the page is #faf8f4, cream; white on red */
#root { display: block; }
.tt-a { content: "#fff white"; white-space: nowrap; background: var(--color-bg) url("data:image/svg+xml,%3Csvg fill='red'%3E"); }
.tt-b { color: var(--tt-red-text, currentColor); border-color: transparent; transition: background-color 0.1s; }
`,
  })
  expect(r.err).toBe('')
  expect(r.code).toBe(0)
})

test('a colour in an at-rule that holds declarations is looked for too', () => {
  const r = runFiles({
    'ui/shade.css': '@property --shade {\n  syntax: "<color>";\n  inherits: false;\n  initial-value: #fff;\n}\n',
  })
  expect(r.code).toBe(1)
  expect(r.err).toContain('src/ui/shade.css:4: initial-value has the colour literal #fff')
})

test("a colour's name is an identifier where the author names things, but a hex colour is not", () => {
  const ok = runFiles({
    'ui/anim.css':
      '@keyframes red { to { opacity: 1; } }\n.tt-a { animation: red var(--duration-base); animation-name: red; grid-area: teal; }\n',
  })
  expect(ok.err).toBe('')
  expect(ok.code).toBe(0)
  const hex = runFiles({ 'ui/anim.css': '.tt-a { transition: color 1s, outline-color 1s #abc; }\n' })
  expect(hex.code).toBe(1)
  expect(hex.err).toContain('transition has the colour literal #abc')
})

test("a colour's name inside a function's arguments is a colour, even where names are identifiers", () => {
  const ok = runFiles({
    'ui/list.css': '.tt-a { list-style: upper-roman inside; animation-name: var(--motion, red); }\n',
  })
  expect(ok.err).toBe('')
  expect(ok.code).toBe(0)
  const gradient = runFiles({ 'ui/list.css': '.tt-a { list-style: linear-gradient(red, blue); }\n' })
  expect(gradient.code).toBe(1)
  expect(gradient.err).toContain('list-style has the colour literal red')
  const stops = runFiles({
    'ui/list.css': '.tt-a { list-style: linear-gradient(var(--start, red), var(--end, blue)); }\n',
  })
  expect(stops.code).toBe(1)
  expect(stops.err).toContain('list-style has the colour literal red')
})

test("checks the design system's ui- classes as it checks tt- ones", () => {
  const twice = run('.ui-button { color: inherit; }\n.ui-button { margin: 0; }\n')
  expect(twice.code).toBe(1)
  expect(twice.err).toContain('.ui-button is declared again')
  const unstyled = run('.ui-button { color: inherit; }\n', { 'A.tsx': component('"ui-button ui-missing"') })
  expect(unstyled.code).toBe(1)
  expect(unstyled.err).toContain('ui-missing is used but no rule')
  expect(unstyled.err).not.toContain('ui-button is used')
  // A custom property that happens to start like a class is not one.
  const prop = run('.ui-x { --ui-y: 0; }\n', { 'A.tsx': component('"ui-x"') })
  expect(prop.code).toBe(0)
})

test('a system colour is a colour literal too, outside a theme', () => {
  for (const [value, literal] of [
    ['Canvas', 'Canvas'],
    ['1px solid ButtonBorder', 'ButtonBorder'],
    ['light-dark(CanvasText, var(--color-text))', 'CanvasText'],
    ['highlighttext', 'highlighttext'],
  ]) {
    const r = runFiles({ 'ui/box.css': `.tt-box {\n  color: ${value};\n}\n` })
    expect(r.code, value).toBe(1)
    expect(r.err, value).toContain(`src/ui/box.css:2: color has the colour literal ${literal}`)
  }
  const theme = runFiles({ 'ui/themes/brand.css': '@layer themes { :root { --color-bg: Canvas; } }\n' })
  expect(theme.err).toBe('')
  expect(theme.code).toBe(0)
})

test("a selector one screen declares, declared again in another screen's stylesheet, is declared twice", () => {
  const r = runFiles({
    'boards/boards.legacy.css': '.tt-card { margin: 0; }\n',
    'apps/apps.legacy.css': '\n\n.tt-card { padding: 0; }\n',
  })
  expect(r.code).toBe(1)
  expect(r.err).toContain(
    'src/boards/boards.legacy.css:1: .tt-card is declared again (first at src/apps/apps.legacy.css:3)',
  )
})

describe('in the design system, a size, a space, a radius, a shadow and a duration are tokens', () => {
  test('a literal one fails, wherever it is in the value', () => {
    for (const [property, value, literal] of [
      ['font-size', '15px', '15px'],
      ['font', '600 0.9rem / 1.4 var(--font-sans)', '0.9rem'],
      ['margin-block-start', 'var(--space-2) 0.5rem', '0.5rem'],
      ['padding', 'max(1rem, env(safe-area-inset-top))', '1rem'],
      ['gap', 'var(--space-gap, 6px)', '6px'],
      ['padding-block-start', 'env(safe-area-inset-top, 0.5rem)', '0.5rem'],
      ['padding-inline', 'max(var(--space-2), env(safe-area-inset-left, var(--x, 3px)))', '3px'],
      ['border-start-start-radius', '8px', '8px'],
      ['box-shadow', '0 2px 0 var(--color-accent-edge)', '2px'],
      ['transition', 'transform 0.14s ease', '0.14s'],
      ['animation-duration', '200ms', '200ms'],
    ]) {
      const r = runFiles({ 'ui/Box.css': `@layer components {\n  .ui-box {\n    ${property}: ${value};\n  }\n}\n` })
      expect(r.code, `${property}: ${value}`).toBe(1)
      expect(r.err, `${property}: ${value}`).toContain(`src/ui/Box.css:3: ${property} has the literal ${literal}`)
    }
  })

  test('tokens, the literals with a reason, unitless numbers and the safe areas pass', () => {
    const r = runFiles({
      'ui/Box.css': `@layer components {
  .ui-box {
    font: var(--type-ui);
    font-size: var(--text-md);
    padding: var(--space-2) 0;
    margin: 0px calc(var(--space-4) * 2) 1px;
    padding-block-end: max(var(--space-4), env(safe-area-inset-bottom));
    border-radius: 50%;
    inline-size: 37px;
    box-shadow: var(--shadow-pop);
    transition: transform var(--duration-fast) var(--ease-out);
    gap: 100%;
  }
}
`,
    })
    expect(r.err).toBe('')
    expect(r.code).toBe(0)
  })

  test('the files that define the tokens define them with literals, and the screens not yet rebuilt may write them', () => {
    const r = runFiles({
      'ui/tokens.css': '@layer tokens { :root { --space-1: 4px; --duration-fast: 120ms; } }\n',
      'ui/legacy.css': '@layer tokens { :root { --tt-radius: 8px; } }\n',
      'ui/themes/brand.css': '@layer themes { :root { --color-x: Canvas; } }\n',
      'boards/boards.legacy.css': '.tt-card { padding: 0.6rem; border-radius: 8px; font-size: 0.85rem; }\n',
    })
    expect(r.err).toBe('')
    expect(r.code).toBe(0)
  })

  test("a rule in the files that define the tokens is held to them too; only the definitions aren't", () => {
    // (A theme is held to colour roles alone by its own test, ui/tokens.test.ts.)
    for (const file of ['ui/tokens.css', 'ui/legacy.css']) {
      const r = runFiles({ [file]: '@layer tokens {\n  :root { --space-1: 4px; }\n  .ui-x { padding: 4px; }\n}\n' })
      expect(r.code, file).toBe(1)
      expect(r.err, file).toContain(`src/${file}:3: padding has the literal 4px`)
    }
  })

  test('a custom property of a component holds a token, unless LOCAL_LITERALS gives the reason', () => {
    const r = runFiles({ 'ui/Box.css': '.ui-box {\n  --ui-box-gap: 6px;\n  --ui-box-in: -100%;\n}\n' })
    expect(r.code).toBe(1)
    expect(r.err).toContain('src/ui/Box.css:2: --ui-box-gap has the literal 6px')
    expect(r.err).not.toContain('-100%')
    const allowed = runFiles({ 'ui/a2ui.css': ':root {\n  --a2ui-button-padding: 0.4rem 1rem;\n}\n' })
    expect(allowed.err).toBe('')
    expect(allowed.code).toBe(0)
  })

  test("a registered property's initial value is held to the rule as a custom property's is", () => {
    const r = runFiles({
      'ui/Box.css': '@property --ui-gap {\n  syntax: "<length>";\n  inherits: false;\n  initial-value: 6px;\n}\n',
    })
    expect(r.code).toBe(1)
    expect(r.err).toContain('src/ui/Box.css:4: initial-value has the literal 6px')
    const zero = runFiles({
      'ui/Box.css': '@property --ui-gap {\n  syntax: "<length>";\n  inherits: false;\n  initial-value: 0px;\n}\n',
    })
    expect(zero.err).toBe('')
  })

  test('the vendor-prefixed and older names of a scale property are checked too', () => {
    for (const [property, literal] of [
      ['-webkit-box-shadow', '2px'],
      ['grid-gap', '6px'],
      ['scroll-margin-block-start', '6px'],
      ['scroll-padding', '6px'],
    ]) {
      const r = runFiles({ 'ui/Box.css': `.ui-box {\n  ${property}: 0 ${literal};\n}\n` })
      expect(r.code, property).toBe(1)
      expect(r.err, property).toContain(`${property} has the literal ${literal}`)
    }
    const hairline = runFiles({ 'ui/Box.css': '.ui-box {\n  margin: -1px;\n}\n' })
    expect(hairline.err).toBe('')
  })
})

// A whole app's stylesheets, as main.tsx loads them: the entry, ui/index.css, the list of the old
// stylesheets with each of them, and what `changes` adds, replaces or (with null) removes.
const OPT = ':not(:where([class^="ui-"], [class*=" ui-"]))'
const GOOD_BASE = `@layer base {\n  a${OPT} { color: inherit; }\n}\n\n@media (pointer: coarse) {\n  label${OPT} { min-height: var(--tt-touch); }\n}\n`
function app(changes: Record<string, string | null> = {}) {
  const files: Record<string, string | null> = {
    'main.tsx': "import './ui/index.css'\nimport { App } from './App'\nimport './styles.css'\n",
    'ui/index.css': '@layer tokens, themes, base, components, screens;\n',
    'styles.css': `/* the list */\n${OLD_STYLESHEETS.map((f) => `@import "./${f}";`).join('\n')}\n`,
    'App.tsx': 'export const App = () => null\n',
    ...Object.fromEntries(OLD_STYLESHEETS.map((f) => [f, ''])),
    'ui/base.css': GOOD_BASE,
    ...changes,
  }
  const root = mkdtempSync(join(tmpdir(), 'css-check-'))
  dirs.push(root)
  for (const [name, text] of Object.entries(files)) {
    if (text === null) continue
    mkdirSync(join(root, 'src', name, '..'), { recursive: true })
    writeFileSync(join(root, 'src', name), text)
  }
  const r = spawnSync(process.execPath, [script, '--root', root], { encoding: 'utf8' })
  return { code: r.status, out: r.stdout, err: r.stderr }
}

describe('how the app loads its stylesheets', () => {
  test('an app that loads each one once, the list in its order, passes', () => {
    const r = app({
      'App.tsx':
        "import './home/Widget'\nimport { I } from './ui/Input'\nconst S = import('./ui/Select')\nexport const App = () => [I, S]\n",
      'home/Widget.tsx': "import './Widget.css'\nexport const W = () => null\n",
      'home/Widget.css': '@layer screens { .tt-widget-x { color: inherit; } }\n',
      'ui/Input.tsx': "import './Input.css'\n",
      'ui/Select.tsx': "import './Input.css'\nimport './Select.css'\n",
      'ui/Input.css': '@layer components { .ui-input { color: inherit; } }\n',
      'ui/Select.css': '@layer components { .ui-select { color: inherit; } }\n',
    })
    expect(r.err).toBe('')
    expect(r.code).toBe(0)
  })

  test('the entry imports ui/index.css before anything else', () => {
    const r = app({ 'main.tsx': "import { App } from './App'\nimport './ui/index.css'\nimport './styles.css'\n" })
    expect(r.code).toBe(1)
    expect(r.err).toContain("src/main.tsx: its first import is './App', not './ui/index.css'")
  })

  test('a stylesheet nothing loads, or one the list names that is not there, fails', () => {
    const stray = app({ 'search/extra.css': '.tt-extra { color: inherit; }\n' })
    expect(stray.code).toBe(1)
    expect(stray.err).toContain('src/search/extra.css: nothing loads it')

    const dropped = app({
      'styles.css': OLD_STYLESHEETS.filter((f) => f !== 'markdown.legacy.css')
        .map((f) => `@import "./${f}";`)
        .join('\n'),
    })
    expect(dropped.code).toBe(1)
    expect(dropped.err).toContain('src/markdown.legacy.css: nothing loads it')
    expect(dropped.err).toContain('src/styles.css: lists ')

    const gone = app({ 'settings/settings.legacy.css': null })
    expect(gone.code).toBe(1)
    expect(gone.err).toContain('src/styles.css: lists src/settings/settings.legacy.css, which is not there')
  })

  test('the list counts only if the entry imports it', () => {
    const r = app({ 'main.tsx': "import './ui/index.css'\nimport { App } from './App'\n" })
    expect(r.code).toBe(1)
    expect(r.err).toContain("src/main.tsx: doesn't import './styles.css'")
  })

  test('a stylesheet ui/index.css imports and a module imports too is loaded twice', () => {
    const r = app({
      'ui/index.css': '@layer tokens, themes, base, components, screens;\n@import "./Box.css";\n',
      'App.tsx': "import './ui/Box'\n",
      'ui/Box.tsx': "import './Box.css'\n",
      'ui/Box.css': '@layer components { .ui-box { color: inherit; } }\n',
    })
    expect(r.code).toBe(1)
    expect(r.err).toContain('src/ui/Box.css: loaded more than one way')
  })

  test('only a module the app reaches from its entry loads a stylesheet, a glob of the gallery included', () => {
    const files = {
      'home/Widget.tsx': "import './Widget.css'\n",
      'home/Widget.css': '@layer screens { .tt-widget-x { color: inherit; } }\n',
    }
    const dead = app(files)
    expect(dead.code).toBe(1)
    expect(dead.err).toContain('src/home/Widget.css: nothing loads it')
    const glob = app({
      ...files,
      'App.tsx': "const G = import.meta.glob('./home/*.tsx')\nexport const App = () => G\n",
    })
    expect(glob.err).toBe('')
    expect(glob.code).toBe(0)
  })

  test('the list may write a url() with or without quotes', () => {
    const r = app({
      'styles.css': OLD_STYLESHEETS.map((f, i) => (i % 2 ? `@import url(./${f});` : `@import url("./${f}");`)).join(
        '\n',
      ),
    })
    expect(r.err).toBe('')
    expect(r.code).toBe(0)
  })

  test('ui/index.css begins with the layers in their order and imports its files with no condition', () => {
    const order = app({ 'ui/index.css': '@layer tokens, themes, components, base, screens;\n' })
    expect(order.code).toBe(1)
    expect(order.err).toContain(
      'src/ui/index.css: its first statement is `@layer tokens, themes, base, components, screens;`',
    )
    const printed = app({
      'ui/index.css': '@layer tokens, themes, base, components, screens;\n@import "./Box.css" print;\n',
      'ui/Box.css': '@layer components { .ui-box { color: inherit; } }\n',
    })
    expect(printed.code).toBe(1)
    expect(printed.err).toContain('src/ui/index.css:2: imports ./Box.css with `print`')
  })

  test('a stylesheet both listed and imported by a module is loaded twice', () => {
    const r = app({ 'App.tsx': "import './Md'\n", 'Md.tsx': "import './markdown.legacy.css'\n" })
    expect(r.code).toBe(1)
    expect(r.err).toContain('src/markdown.legacy.css: loaded more than one way')
  })

  test('the list is in its order, plain, and nothing else', () => {
    const moved = [...OLD_STYLESHEETS.filter((f) => f !== 'shell/shell.legacy.css'), 'shell/shell.legacy.css']
    const order = app({ 'styles.css': moved.map((f) => `@import "./${f}";`).join('\n') })
    expect(order.code).toBe(1)
    expect(order.err).toContain('the old stylesheets are, in this order')

    const layered = app({
      'styles.css': OLD_STYLESHEETS.map(
        (f) => `@import "./${f}"${f === 'legacy-shared.css' ? ' layer(screens)' : ''};`,
      ).join('\n'),
    })
    expect(layered.code).toBe(1)
    expect(layered.err).toContain('imports ./legacy-shared.css with `layer(screens)`')

    const rule = app({
      'styles.css': `${OLD_STYLESHEETS.map((f) => `@import "./${f}";`).join('\n')}\n.tt-late { color: inherit; }\n`,
    })
    expect(rule.code).toBe(1)
    expect(rule.err).toContain('holds only @imports of the old stylesheets, not `.tt-late`')
  })

  test("an old screen's file declares no layer, nested in @media either", () => {
    const file = 'markdown.legacy.css'
    const r = app({ [file]: '@layer screens {\n  .tt-app { color: inherit; }\n}\n' })
    expect(r.code).toBe(1)
    expect(r.err).toContain(`src/${file}:1: declares \`@layer screens\``)
    const nested = app({
      [file]:
        '/* @layer in a comment */\n.tt-a::before { content: "@layer x"; }\n@media (min-width: 1px) {\n  @layer screens { .tt-app { color: inherit; } }\n}\n',
    })
    expect(nested.code).toBe(1)
    expect(nested.err).toContain(`src/${file}:4: declares \`@layer screens\``)
    expect(nested.err).not.toContain('@layer x')
    expect(nested.err).not.toContain(`${file}:1:`)
  })

  test('ui/base.css is one @layer base block, then what reaches everything on purpose, then the touch block', () => {
    for (const base of [
      // The touch screen's block moved into the layer.
      `@layer base {\n  a${OPT} { color: inherit; }\n  @media (pointer: coarse) { label${OPT} { min-height: 0; } }\n}\n`,
      // The base in another layer.
      GOOD_BASE.replace('@layer base', '@layer screens'),
      // A rule after the blocks, in no layer.
      `${GOOD_BASE}\nfieldset${OPT} { margin: 0; }\n`,
    ]) {
      const r = app({ 'ui/base.css': base })
      expect(r.code, base).toBe(1)
      expect(r.err, base).toContain(
        'src/ui/base.css: is one `@layer base` block, then in no layer the rules that reach every element on purpose',
      )
    }
  })

  test('in no layer, ui/base.css holds only what reaches every element on purpose', () => {
    const middle = (rule: string) =>
      GOOD_BASE.replace('\n\n@media (pointer: coarse)', `\n\n${rule}\n\n@media (pointer: coarse)`)
    const everywhere = app({
      'ui/base.css': middle(
        '::selection { background: var(--tt-accent); }\n@media (max-width: 48rem) { input { font-size: var(--tt-text-no-zoom); } }',
      ),
    })
    expect(everywhere.err).toBe('')
    expect(everywhere.code).toBe(0)
    for (const rule of [
      `p${OPT} { text-wrap: pretty; }`,
      '::selection { margin: 0; }',
      '.tt-muted { color: inherit; }',
    ]) {
      const r = app({ 'ui/base.css': middle(rule) })
      expect(r.code, rule).toBe(1)
      expect(r.err, rule).toContain('is in no layer: only what reaches every element on purpose (EVERYWHERE) is')
    }
  })

  test("ui/base.css's first part is a @layer base block, not a statement", () => {
    const r = app({
      'ui/base.css': `@layer base;\n\n@media (pointer: coarse) {\n  label${OPT} { min-height: var(--tt-touch); }\n}\n`,
    })
    expect(r.code).toBe(1)
    expect(r.err).toContain('src/ui/base.css: is one `@layer base` block')
  })

  test('a comment marker inside a string is not a comment', () => {
    const base = GOOD_BASE.replace(
      `  a${OPT} { color: inherit; }\n`,
      `  a${OPT} { color: inherit; }\n  .tt-a::before { content: "/*"; }\n`,
    ).replace(
      `  label${OPT} { min-height: var(--tt-touch); }\n`,
      `  label${OPT} { min-height: var(--tt-touch); }\n  label${OPT}::after { content: "*/"; }\n`,
    )
    const r = app({ 'ui/base.css': base })
    expect(r.err).toBe('')
    expect(r.code).toBe(0)
  })

  test('a brace inside a string or escaped is not the end of a block of ui/base.css', () => {
    const base = GOOD_BASE.replace(
      `  a${OPT} { color: inherit; }\n`,
      `  a${OPT} { color: inherit; }\n  .tt-mark::before { content: "}"; }\n  .tt-open::after { content: '{'; }\n  .tt-esc::before { content: "\\\\}"; }\n`,
    )
    const r = app({ 'ui/base.css': base })
    expect(r.err).toBe('')
    expect(r.code).toBe(0)
  })

  test("ui/base.css's element rules leave the components' elements out, in the layer and outside it", () => {
    for (const base of [GOOD_BASE.replace(`a${OPT}`, 'a'), GOOD_BASE.replace(`label${OPT}`, 'label')]) {
      const r = app({ 'ui/base.css': base })
      expect(r.code, base).toBe(1)
      expect(r.err, base).toContain("reaches into the design system's components")
    }
  })
})

describe('nested rules in an old stylesheet', () => {
  // What css-check makes of each rule: whether it reaches anywhere, and if so whether it may.
  const verdicts = (css: string) =>
    allRules(css).map((r) => ({
      selector: r.selector,
      properties: r.properties,
      refused: reachesAnywhere(r.selector) && !allowedEverywhere(r),
    }))

  test('a nested rule is scoped by its parent, with & or without it', () => {
    expect(
      verdicts('.tt-card { color: red; & h2 { margin: 0; } &:hover { opacity: 0.8; } h3 { margin: 0; } }'),
    ).toEqual([
      { selector: '.tt-card', properties: ['color'], refused: false },
      { selector: '.tt-card h2', properties: ['margin'], refused: false },
      { selector: '.tt-card:hover', properties: ['opacity'], refused: false },
      { selector: '.tt-card h3', properties: ['margin'], refused: false },
    ])
    // An ampersand in a string or escaped is a character, not the parent.
    expect(allRules('.tt-card { a[href*="&"] { margin: 0; } .x\\&y { margin: 0; } }').map((r) => r.selector)).toEqual([
      '.tt-card',
      '.tt-card a[href*="&"]',
      '.tt-card .x\\&y',
    ])
    // After a compound's start, the parent is a condition on that element, and a parent of
    // wrappers alone scopes nothing.
    // (Glued on as text, `#tt-main&` would be `#tt-mainbody #root input`, an id of its own that
    // seems to scope it.)
    expect(verdicts('body #root { #tt-main& input { margin: 0; } }').at(-1)).toEqual({
      selector: '#tt-main:is(body #root) input',
      properties: ['margin'],
      refused: true,
    })
    // A leading combinator keeps the parent in front, even with an & later on.
    expect(allRules('.tt-card { + .x + & { margin: 0; } }').map((r) => r.selector)).toEqual([
      '.tt-card',
      '.tt-card + .x + .tt-card',
    ])
    // Each & is the whole parent list, chosen on its own.
    expect(allRules('.tt-a, .tt-b { & + & { margin: 0; } }').map((r) => r.selector)).toEqual([
      '.tt-a',
      '.tt-b',
      '.tt-a + .tt-a',
      '.tt-a + .tt-b',
      '.tt-b + .tt-a',
      '.tt-b + .tt-b',
    ])
    // Each of several parents.
    expect(allRules('.tt-a, .tt-b { & p { margin: 0; } }').map((r) => r.selector)).toEqual([
      '.tt-a',
      '.tt-b',
      '.tt-a p',
      '.tt-b p',
    ])
  })

  test('a nested rule under a rule that reaches anywhere reaches anywhere too', () => {
    expect(verdicts('main { & h2 { margin: 0; } }').at(-1)).toEqual({
      selector: 'main h2',
      properties: ['margin'],
      refused: true,
    })
    expect(verdicts(`h2${OPT_OUT} { &:hover { color: red; } }`).at(-1)?.refused).toBe(false)
  })

  test("a conditional rule's declarations nested in a style rule are that rule's", () => {
    expect(verdicts('input { @media (pointer: coarse) { padding: 0; } }')).toEqual([
      { selector: 'input', properties: ['padding'], refused: true },
    ])
    expect(verdicts('input { @media (max-width: 40rem) { @supports (x: y) { font-size: 16px; } } }')).toEqual([
      { selector: 'input', properties: ['font-size'], refused: false },
    ])
    // At the top, a @media's rules are what they are, as before.
    expect(verdicts('@media (pointer: coarse) { h2 { margin: 0; } .tt-x { margin: 0; } }')).toEqual([
      { selector: 'h2', properties: ['margin'], refused: true },
      { selector: '.tt-x', properties: ['margin'], refused: false },
    ])
  })

  test('a class named only in a nested selector, with & or without it, has a rule', () => {
    const r = run(
      '.tt-card { margin: 0; & .tt-inner { padding: 0; } &.tt-on { color: inherit; } .tt-plain { margin: 0; } }\n',
      { 'A.tsx': 'export const A = () => <p className="tt-card tt-inner tt-on tt-plain">x</p>\n' },
    )
    expect(r.err).toBe('')
    expect(r.code).toBe(0)
  })

  test('inside @keyframes, nothing is a rule', () => {
    expect(allRules('@keyframes spin { from { rotate: 0; } to { rotate: 1turn; } }')).toEqual([])
  })
})
