import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { afterEach, expect, test } from 'vitest'
import { unusedAliasErrors, unusedClassErrors } from '../scripts/css-check.mjs'

const dirs: string[] = []
afterEach(() => {
  for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true })
})

function web(files: Record<string, string>) {
  const root = mkdtempSync(join(tmpdir(), 'css-unused-'))
  dirs.push(root)
  for (const [name, text] of Object.entries(files)) {
    mkdirSync(dirname(join(root, name)), { recursive: true })
    writeFileSync(join(root, name), text)
  }
  return root
}

test('the app as it is has no dead old rule', () => {
  expect(unusedClassErrors(resolve(__dirname, '..'))).toEqual([])
})

test('a rule whose class no module uses fails, once per class, with its line', () => {
  const root = web({
    'src/old.legacy.css':
      '.tt-live { color: inherit; }\n\n.tt-gone { margin: 0; }\n.tt-live .tt-gone { padding: 0; }\n',
    'src/Live.tsx': 'export const L = () => <p className="tt-live" />\n',
  })
  expect(unusedClassErrors(root)).toEqual([
    'src/old.legacy.css:3: .tt-gone has a rule but nothing in the app uses the class: delete its rules (or, if a module builds the name from pieces, add it to BUILT_FROM_PIECES with where)',
  ])
})

test('a class named only in a nested rule is found unused like any other, with & or without it', () => {
  const root = web({
    'src/old.legacy.css': [
      '.tt-card {',
      '  margin: 0;',
      '  & .tt-inner { padding: 0; }',
      '  &.tt-on { color: inherit; }',
      '  .tt-plain { margin: 0; }',
      '  @media (max-width: 40rem) { &:is(.tt-narrow) { margin: 0; } }',
      '}',
      '',
    ].join('\n'),
    'src/Card.tsx': 'export const C = () => <p className="tt-card tt-inner" />\n',
  })
  const unused = (cls: string, line: number) =>
    `src/old.legacy.css:${line}: .${cls} has a rule but nothing in the app uses the class: delete its rules (or, if a module builds the name from pieces, add it to BUILT_FROM_PIECES with where)`
  expect(unusedClassErrors(root)).toEqual([unused('tt-on', 4), unused('tt-plain', 5), unused('tt-narrow', 6)])
})

test('a comment, a test or the gallery does not keep a rule alive, nor a longer name', () => {
  const root = web({
    'src/old.legacy.css': '.tt-x { margin: 0; }\n',
    'src/A.tsx': '// className="tt-x"\nexport const A = () => <p className="tt-x-y" />\n',
    'src/A.test.tsx': 'render(<p className="tt-x" />)\n',
    'src/ui/A.gallery.tsx': 'export default () => <p className="tt-x" />\n',
  })
  expect(unusedClassErrors(root)).toHaveLength(1)
})

test('a class in a lookup table, a string or index.html keeps its rule; a ui- class is not checked', () => {
  const root = web({
    'src/old.legacy.css': '.tt-a, .tt-b, .tt-c { margin: 0; }\n.ui-built-from-pieces { margin: 0; }\n',
    'src/A.tsx': "const KIND = { a: 'tt-a' }\nexport const cls = `tt-b`\n",
    'index.html': '<body class="tt-c"></body>\n',
  })
  expect(unusedClassErrors(root)).toEqual([])
})

test('a name in a declaration’s value, an attribute selector or a comment is not a rule for the class', () => {
  const root = web({
    'src/old.legacy.css':
      '/* .tt-in-a-comment { margin: 0; } */\n.ui-x::before { content: ".tt-in-a-string"; }\n[data-kind="tt-in-an-attribute"] { margin: 0; }\n@media (width > 1px) {\n  .tt-nested { margin: 0; }\n}\n',
  })
  expect(unusedClassErrors(root)).toEqual([
    'src/old.legacy.css:5: .tt-nested has a rule but nothing in the app uses the class: delete its rules (or, if a module builds the name from pieces, add it to BUILT_FROM_PIECES with where)',
  ])
})

test('a class only in an HTML comment of index.html keeps no rule alive', () => {
  const root = web({
    'src/old.legacy.css': '.tt-commented { margin: 0; }\n.tt-real { margin: 0; }\n',
    'index.html': '<!-- <div class="tt-commented"></div> -->\n<body class="tt-real"></body>\n',
  })
  expect(unusedClassErrors(root)).toEqual([
    'src/old.legacy.css:1: .tt-commented has a rule but nothing in the app uses the class: delete its rules (or, if a module builds the name from pieces, add it to BUILT_FROM_PIECES with where)',
  ])
})

test('a class an end-to-end harness draws keeps its rule; a class built from pieces can be named', () => {
  const root = web({
    'src/old.legacy.css': '.tt-harness { margin: 0; }\n.tt-built { margin: 0; }\n',
    'e2e/conversation/harness.tsx': 'export const H = () => <main className="tt-harness" />\n',
  })
  expect(unusedClassErrors(root, new Map([['tt-built', 'src/A.tsx builds it']]))).toEqual([])
  expect(unusedClassErrors(root)[0]).toContain('.tt-built has a rule but nothing in the app uses the class')
  expect(unusedClassErrors(root, new Map([['tt-gone', 'no longer']]))).toContain(
    'tt-gone is in BUILT_FROM_PIECES but no rule names it: remove it from the list',
  )
})

test('an old --tt-* alias nothing reads fails, and one read by a stylesheet or a module passes', () => {
  const root = web({
    'src/ui/legacy.css':
      '@layer tokens {\n  :root {\n    --tt-read: var(--color-bg);\n    --tt-scripted: 1px;\n    --tt-own-message: var(--color-surface-own);\n    /* --tt-in-a-comment: 0; */\n  }\n}\n',
    'src/old.legacy.css': '.tt-a { color: var(--tt-read); }\n',
    'src/A.tsx': "document.documentElement.style.setProperty('--tt-scripted', '2px')\n// --tt-own-message\n",
  })
  expect(unusedAliasErrors(root)).toEqual([
    'src/ui/legacy.css:5: --tt-own-message is defined but nothing reads it: delete it',
  ])
})

test('the app as it is reads every alias it keeps', () => {
  expect(unusedAliasErrors(resolve(__dirname, '..'))).toEqual([])
})

test('an alias another stylesheet only sets again is not read', () => {
  const root = web({
    'src/ui/legacy.css': '@layer tokens {\n  :root {\n    --tt-overridden: 0;\n  }\n}\n',
    'src/old.legacy.css': '.tt-a { --tt-overridden: 1px; }\n',
  })
  expect(unusedAliasErrors(root)).toEqual([
    'src/ui/legacy.css:3: --tt-overridden is defined but nothing reads it: delete it',
  ])
})
