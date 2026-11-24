import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { expect, test } from 'vitest'
import { PLACEHOLDER, precacheEntries, pwa, renderServiceWorker } from './plugin'

const template = `const VERSION = ${PLACEHOLDER.version}\nconst PRECACHE = ${PLACEHOLDER.precache}\n`

// A build's files: the page, hashed chunks (the lazy ones too), a language file's chunk,
// the public/ files, and what the worker leaves out.
const build = [
  'index.html',
  'manifest.webmanifest',
  'icons/icon-192.png',
  'icons/icon.svg',
  'assets/index-abc123.js',
  'assets/index-abc123.css',
  'assets/AppsPage-def456.js', // the lazy-loaded apps renderer
  'assets/pt-BR-ghi789.js',
  'assets/index-abc123.js.map',
  'assets/zxing_reader-jkl012.wasm', // the scanner's decoder
  'assets/zxing_writer-mno345.wasm', // the QR writer's
  'vendor/novnc/core/rfb.js',
  'assets/THIRD_PARTY_NOTICES.txt',
  'sw.js',
]

test('precaches the page, public files and every chunk, lazy ones included', () => {
  const entries = precacheEntries(build)
  expect(entries).toContain('/index.html')
  expect(entries).toContain('/manifest.webmanifest')
  expect(entries).toContain('/icons/icon-192.png')
  expect(entries).toContain('/assets/AppsPage-def456.js')
  expect(entries).toContain('/assets/pt-BR-ghi789.js')
  expect(entries).toContain('/assets/index-abc123.css')
  // Scanning works offline, so its decoder is kept.
  expect(entries).toContain('/assets/zxing_reader-jkl012.wasm')
})

test("leaves out the QR writer's .wasm: drawing the two-factor secret needs the server anyway", () => {
  expect(precacheEntries(build).some((e) => e.includes('zxing_writer'))).toBe(false)
})

test('leaves out the worker itself, source maps, the vendored live view and the licence notices', () => {
  const entries = precacheEntries(build)
  expect(entries).not.toContain('/sw.js')
  expect(entries.some((e) => e.endsWith('.map'))).toBe(false)
  expect(entries.some((e) => e.startsWith('/vendor/'))).toBe(false)
  expect(entries.some((e) => e.endsWith('THIRD_PARTY_NOTICES.txt'))).toBe(false)
})

test('a build without the page is an error', () => {
  expect(() => precacheEntries(['assets/a.js'])).toThrow(/index\.html/)
})

test('fills the template in, and refuses one without its markers', () => {
  const out = renderServiceWorker(template, ['/index.html'], 'v1')
  expect(out).toContain('const VERSION = "v1"')
  expect(out).toContain('"/index.html"')
  expect(() => renderServiceWorker('nothing', [], 'v')).toThrow(/lacks/)
})

test('the plugin writes sw.js from the built files, and its version follows their content', () => {
  const dir = mkdtempSync(join(tmpdir(), 'taskto-pwa-'))
  const write = (name: string, body: string) => {
    mkdirSync(join(dir, name, '..'), { recursive: true })
    writeFileSync(join(dir, name), body)
  }
  const tmpl = join(dir, 'template.js')
  writeFileSync(tmpl, template)
  const out = join(dir, 'dist')
  const run = () => {
    const plugin = pwa(tmpl)
    ;(plugin.configResolved as (c: unknown) => void)({ root: dir, build: { outDir: 'dist' } })
    const close = plugin.closeBundle as { handler: () => void }
    close.handler()
    return readFileSync(join(out, 'sw.js'), 'utf8')
  }
  mkdirSync(out)
  const outWrite = (name: string, body: string) => write(join('dist', name), body)
  outWrite('index.html', '<html>')
  outWrite('assets/index-aaa.js', '1')
  outWrite('assets/Lazy-bbb.js', '2')
  const first = run()
  expect(first).toContain('/assets/Lazy-bbb.js')
  expect(run()).toBe(first) // stable for the same build (sw.js itself isn't an input)
  outWrite('index.html', '<html lang="en">')
  expect(run()).not.toBe(first)
})
