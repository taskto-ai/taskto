import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { afterEach, expect, test } from 'vitest'

// scripts/css-check.mjs: the stylesheet is one global file, so a selector declared twice, or a
// class a component uses that no rule has, are caught here, not on a screen.
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
.tt-card { color: red; }
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
  const twice = run('.tt-snippet { color: red; }\n.tt-other { color: red; }\n.tt-snippet { display: grid; }\n')
  expect(twice.code).toBe(1)
  expect(twice.err).toContain('src/styles.css:3: .tt-snippet is declared again (first at line 1)')

  const grouped = run('.tt-a, .tt-b { color: red; }\n.tt-b { margin: 0; }\n')
  expect(grouped.code).toBe(1)
  expect(grouped.err).toContain('.tt-b is declared again')
  expect(grouped.err).not.toContain('.tt-a is declared again')
})

test('a selector in an at-rule block, or with another selector in front of it, is not the same one', () => {
  const r = run(
    '.tt-a { color: red; }\n.tt-a:hover { color: blue; }\n.tt-x .tt-a { margin: 0; }\n@media print { .tt-a { color: black; } }\n',
  )
  expect(r.err).toBe('')
  expect(r.code).toBe(0)
})

test('fails on a tt- class a component uses that has no rule, in a string, a template or a conditional', () => {
  const css = '.tt-known { color: red; }\n'
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
  const r = run('.tt-known { color: red; }\n', {
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
  const css = readFileSync(join(web, 'src/styles.css'), 'utf8')
  const rules = [...css.matchAll(/(^|\n)\.tt-snippet \{([^}]*)\}/g)]
  // A search snippet is running text: a grid would make each highlighted word a row of its own.
  expect(rules).toHaveLength(1)
  expect(rules[0][2]).not.toMatch(/display:\s*grid/)
  expect(css).toMatch(/\n\.tt-setup-snippet \{[^}]*display:\s*grid/)
})

test('the settings code sample keeps the colour and the wrapping the shared snippet class once gave it', () => {
  const css = readFileSync(join(web, 'src/styles.css'), 'utf8')
  const rule = css.match(/(^|\n)\.tt-setup-snippet \{([^}]*)\}/)?.[2] ?? ''
  // The headings and commands there were muted and wrapped anywhere, before the class was renamed.
  expect(rule).toMatch(/color:\s*var\(--tt-muted\)/)
  expect(rule).toMatch(/overflow-wrap:\s*anywhere/)
})

test('a class a lookup table holds counts as used; an attribute selector is not a rule for a class', () => {
  const css =
    '.tt-known { color: red; }\n[data-state="tt-missing"] { color: blue; }\n.tt-other[data-x=\'tt-also\'] { color: red; }\n'
  const table = `const HEALTH: Record<string, string | undefined> = { none: 'tt-known', bad: 'tt-missing' }
export const A = ({ h }: { h: string }) => <p className={HEALTH[h]}>x</p>
`
  const r = run(css, { 'A.tsx': table })
  expect(r.code).toBe(1)
  expect(r.err).toContain('src/A.tsx:1: tt-missing is used but no rule')
  expect(r.err).not.toContain('tt-known is used')

  // An id in the same file, and a table not in capitals, are not classes it reads.
  const quiet = run('.tt-known { color: red; }\n', {
    'A.tsx': `const lower = { a: 'tt-nothing' }\nexport const A = () => <p id="tt-anchor" className="tt-known">{lower.a}</p>\n`,
  })
  expect(quiet.err).toBe('')
  expect(quiet.code).toBe(0)
})

test('a class is a whole name: `.not-tt-x` is no rule for tt-x, and `not-tt-x` is no use of it', () => {
  const r = run('.not-tt-missing { color: red; }\n.tt-known { color: red; }\n', {
    'A.tsx': component('"tt-known tt-missing"'),
  })
  expect(r.code).toBe(1)
  expect(r.err).toContain('tt-missing is used but no rule')

  const quiet = run('.tt-known { color: red; }\n', { 'A.tsx': component('"tt-known not-tt-else"') })
  expect(quiet.err).toBe('')
  expect(quiet.code).toBe(0)
})

test('a className in a comment uses nothing, and a // in a string is not a comment', () => {
  const r = run('.tt-known { color: red; }\n', {
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
