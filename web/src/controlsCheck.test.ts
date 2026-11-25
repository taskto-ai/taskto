import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { afterEach, expect, test } from 'vitest'
import { ALLOWED, check, controls } from '../scripts/controls-check.mjs'

// scripts/controls-check.mjs: a screen draws its controls with the components of src/ui/, so a
// button, a select, an input or a textarea written by hand anywhere else is caught here.
const web = resolve(__dirname, '..')
const script = join(web, 'scripts/controls-check.mjs')

const dirs: string[] = []
afterEach(() => {
  for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true })
})

// A web directory with these modules under src/, and what the script says of it.
function run(files: Record<string, string>) {
  const root = mkdtempSync(join(tmpdir(), 'controls-check-'))
  dirs.push(root)
  for (const [name, source] of Object.entries(files)) {
    mkdirSync(dirname(join(root, 'src', name)), { recursive: true })
    writeFileSync(join(root, 'src', name), source)
  }
  const r = spawnSync(process.execPath, [script, '--root', root], { encoding: 'utf8' })
  return { code: r.status, err: r.stderr }
}

test('the app as it is passes', () => {
  expect(check(web)).toEqual([])
})

test('a control written by hand in a screen fails, with its file and line', () => {
  const r = run({
    'home/Home.tsx': 'export const X = () => (\n  <div>\n    <button type="button">Go</button>\n  </div>\n)\n',
  })
  expect(r.code).toBe(1)
  expect(r.err).toContain('src/home/Home.tsx:3: a <button> written by hand')
})

test('each of the four elements is caught, and nothing else', () => {
  const found = controls('<input />\n<select>\n<textarea rows={2} />\n<Button>\n<buttons>\n<inputGroup />')
  expect(found.map((c) => c.element)).toEqual(['input', 'select', 'textarea'])
})

test('an element in a string, a template or a regular expression is not one; one after an operator or JSX text is', () => {
  const found = controls(
    [
      "export const html = '<button>Go</button>'",
      'export const page = `<select></select>`',
      'export const tag = /<input/',
      'export const A = () => (',
      '  <div>',
      '    {open && <button type="button" />}',
      '    {open ? <textarea /> : null}',
      '  </div>',
      ')',
      'export const C = () => <div>Save <select /></div>',
      'export function B() {',
      '  return <input />',
      '}',
    ].join('\n'),
  )
  expect(found).toEqual([
    { element: 'button', line: 6 },
    { element: 'textarea', line: 7 },
    { element: 'select', line: 10 },
    { element: 'input', line: 12 },
  ])
})

test('the components, the tests and the gallery may write them, and a comment is not one', () => {
  const r = run({
    'ui/Button.tsx': 'export const B = () => <button type="button" />\n',
    'home/Home.test.tsx': 'render(<button type="button" />)\n',
    'ui/Button.gallery.tsx': 'export default () => <button type="button" />\n',
    'home/Home.tsx': '// a <button> is drawn by Button\nexport const X = () => <div>{/* <input> */}</div>\n',
  })
  expect(r.code).toBe(0)
})

test('an allowed file may have as many as it says, no more, and a count it no longer needs fails', () => {
  const allowed = new Map([['src/boards/BoardCard.tsx', { button: 1, reason: 'test' }]])
  const root = mkdtempSync(join(tmpdir(), 'controls-check-'))
  dirs.push(root)
  mkdirSync(join(root, 'src/boards'), { recursive: true })
  const write = (n: number) =>
    writeFileSync(join(root, 'src/boards/BoardCard.tsx'), '<button type="button" />\n'.repeat(n))
  write(1)
  expect(check(root, allowed)).toEqual([])
  write(2)
  expect(check(root, allowed)[0]).toContain('src/boards/BoardCard.tsx:2: a <button> written by hand')
  write(0)
  expect(check(root, allowed)[0]).toContain('ALLOWED says 1 <button>, the file has 0')
})

test('every exception says why', () => {
  for (const [file, entry] of ALLOWED) expect(entry.reason.length, file).toBeGreaterThan(40)
})
