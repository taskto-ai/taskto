import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { afterEach, expect, test } from 'vitest'
import { unusedClassErrors } from '../scripts/css-check.mjs'

// css-check's reverse check: a rule naming an old `tt-` class that nothing in the app uses is dead
// (its screen moved onto the components), and fails, so the old stylesheets only shrink.
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
    'src/old.legacy.css:3: .tt-gone has a rule but nothing in the app uses the class: delete its rules',
  ])
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
