import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { afterEach, describe, expect, test } from 'vitest'

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

test('the search result snippet and the settings code sample have a class each', () => {
  const search = readFileSync(join(web, 'src/search/search.css'), 'utf8')
  const rules = [...search.matchAll(/(^|\n)\.tt-snippet \{([^}]*)\}/g)]
  // A search snippet is running text: a grid would make each highlighted word a row of its own.
  expect(rules).toHaveLength(1)
  expect(rules[0][2]).not.toMatch(/display:\s*grid/)
  const settings = readFileSync(join(web, 'src/settings/settings.css'), 'utf8')
  expect(settings).toMatch(/\n\.tt-setup-snippet \{[^}]*display:\s*grid/)
})

test('the settings code sample keeps the colour and the wrapping the shared snippet class once gave it', () => {
  const css = readFileSync(join(web, 'src/settings/settings.css'), 'utf8')
  const rule = css.match(/(^|\n)\.tt-setup-snippet \{([^}]*)\}/)?.[2] ?? ''
  // The headings and commands there were muted and wrapped anywhere, before the class was renamed.
  expect(rule).toMatch(/color:\s*var\(--tt-muted\)/)
  expect(rule).toMatch(/overflow-wrap:\s*anywhere/)
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

test('a selector one screen declares, declared again in another screen\'s stylesheet, is declared twice', () => {
  const r = runFiles({
    'boards/boards.css': '.tt-card { margin: 0; }\n',
    'apps/apps.css': '\n\n.tt-card { padding: 0; }\n',
  })
  expect(r.code).toBe(1)
  expect(r.err).toContain('src/boards/boards.css:1: .tt-card is declared again (first at src/apps/apps.css:3)')
})

describe('in the design system, a size, a space, a radius, a shadow and a duration are tokens', () => {
  test('a literal one fails, wherever it is in the value', () => {
    for (const [property, value, literal] of [
      ['font-size', '15px', '15px'],
      ['font', '600 0.9rem / 1.4 var(--font-sans)', '0.9rem'],
      ['margin-block-start', 'var(--space-2) 0.5rem', '0.5rem'],
      ['padding', 'max(1rem, env(safe-area-inset-top))', '1rem'],
      ['gap', 'var(--space-gap, 6px)', '6px'],
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

  test('the files that define the tokens, and the screens not yet rebuilt, may write literals', () => {
    const r = runFiles({
      'ui/tokens.css': '@layer tokens { :root { --space-1: 4px; } .ui-x { padding: 4px; } }\n',
      'ui/legacy.css': '@layer tokens { :root { --tt-radius: 8px; } .ui-y { border-radius: 8px; } }\n',
      'ui/themes/brand.css': '@layer themes { .ui-z { transition: color 1s; } }\n',
      'boards/boards.css': '.tt-card { padding: 0.6rem; border-radius: 8px; font-size: 0.85rem; }\n',
    })
    expect(r.err).toBe('')
    expect(r.code).toBe(0)
  })
})

test("the app's stylesheets are the list styles.css imports, each file once", () => {
  const entry = readFileSync(join(web, 'src/styles.css'), 'utf8')
  const imports = [...entry.matchAll(/@import "\.\/([^"]+)";/g)].map((m) => m[1])
  expect(new Set(imports).size).toBe(imports.length)
  // Every stylesheet of a screen and the base are in it; the design system's own files come
  // through ui/index.css, and a component's through its component.
  const listed = (path: string) => imports.includes(path)
  for (const path of ['ui/base.css', 'legacy-shared.css', 'markdown.css', 'shell/shell.css', 'apps/apps.css']) {
    expect(listed(path), path).toBe(true)
  }
  // Nothing but the list: a rule here would be one more place every screen writes to.
  expect(entry.replace(/\/\*[\s\S]*?\*\//g, '').replace(/@import "[^"]+";/g, '').trim()).toBe('')
})
