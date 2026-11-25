// @vitest-environment jsdom

import { existsSync, readFileSync, statSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { expect, test } from 'vitest'
import { COMPONENTS, catalogCapabilities, FUNCTIONS } from './capabilities'
import { APPS_CATALOG_ID, appsCatalog } from './catalog'

// capabilities.ts says by name what the catalog holds, so that asking whether a release can be drawn
// doesn't load the renderer. The names must be the catalog's own (apps.test.tsx holds them to the
// server's catalog.json).
test('the names are the catalog’s components and functions', () => {
  expect([...COMPONENTS].sort()).toEqual([...appsCatalog.components.keys()].sort())
  expect([...FUNCTIONS].sort()).toEqual([...appsCatalog.functions.keys()].sort())
  for (const name of appsCatalog.components.keys()) expect(catalogCapabilities.has(`component.${name}`)).toBe(true)
})

const src = resolve(__dirname, '..')

// The modules a module imports statically (not `import type`, not import()), comments left out, so
// a quote in a comment inside a multi-line import can't hide it. A relative import that doesn't
// resolve to a file is an error, not dropped.
function staticImports(code: string): string[] {
  // Block comments, then line comments (a `//` after a colon is a URL's, not one).
  const text = code.replace(/\/\*[\s\S]*?\*\//g, ' ').replace(/(^|[^:])\/\/.*$/gm, '$1')
  return [...text.matchAll(/^\s*(?:import|export)\s+(?!type\b)(?:[^'"]*?\sfrom\s+)?['"]([^'"]+)['"]/gm)].map(
    (m) => m[1],
  )
}

function resolveImport(from: string, spec: string): string {
  if (!spec.startsWith('.')) return spec
  const base = resolve(dirname(from), spec)
  for (const ext of ['', '.tsx', '.ts', '/index.tsx', '/index.ts'])
    if (existsSync(base + ext) && statSync(base + ext).isFile()) return base + ext
  throw new Error(`${relative(src, from)} imports ${spec}, which is no file`)
}

// Every module the app's entry reaches through static imports, each with the module that imported
// it: what every page loads before it shows anything.
function reachedFrom(entry: string, read = (f: string) => readFileSync(f, 'utf8')): Map<string, string | null> {
  const from = new Map<string, string | null>([[entry, null]])
  const queue = [entry]
  while (queue.length > 0) {
    const f = queue.shift() as string
    // A package, a stylesheet or an asset is a leaf.
    if (!f.startsWith('/') || !/\.tsx?$/.test(f)) continue
    for (const spec of staticImports(read(f))) {
      const dep = resolveImport(f, spec)
      if (!from.has(dep)) {
        from.set(dep, relative(src, f))
        queue.push(dep)
      }
    }
  }
  return from
}

// The renderer: A2UI's packages, its schema libraries (with their subpaths, `zod/v3`), and the
// catalog that holds them.
const RENDERER = /^@a2ui\/|^zod(?:\/|$)|^zod-to-json-schema(?:\/|$)|^redos-detector(?:\/|$)|apps\/catalog\.tsx$/

function renderer(reached: Map<string, string | null>): string[] {
  return [...reached.keys()].filter((k) => RENDERER.test(k)).map((k) => `${k} (imported by ${reached.get(k)})`)
}

test('the renderer is loaded only when an app is drawn, never by the entry', () => {
  const reached = reachedFrom(join(src, 'main.tsx'))
  // The proof the walk works: the entry reaches the shell and the offline download.
  expect([...reached.keys()].some((f) => f.endsWith('offline/download.ts'))).toBe(true)
  expect(renderer(reached)).toEqual([])
})

test('the walk sees an import a comment with a quote sits in, and a package’s subpath', () => {
  const entry = join(src, 'main.tsx')
  const download = join(src, 'offline/download.ts')
  const read = (f: string) => {
    if (f === entry) return "import './offline/download'\n"
    if (f === download)
      return "import {\n  // the catalog's own answer\n  unsupportedBy,\n} from '../apps/catalog'\nimport { z } from 'zod/v3'\n"
    return ''
  }
  expect(renderer(reachedFrom(entry, read))).toEqual([
    `${join(src, 'apps/catalog.tsx')} (imported by offline/download.ts)`,
    'zod/v3 (imported by offline/download.ts)',
  ])
})

test('a relative import that is no file fails the walk instead of being dropped', () => {
  const entry = join(src, 'main.tsx')
  expect(() => reachedFrom(entry, () => "import { x } from './no-such-module'\n")).toThrow(
    /no-such-module, which is no file/,
  )
})

// The build itself, where it has been made (CI's web job builds before it tests): no script the
// page links (the entry and what it preloads) holds the catalog. A local dist/ from an older build
// can be stale: build again before trusting this one.
const dist = resolve(src, '../dist')
test.skipIf(!existsSync(join(dist, 'index.html')))('no script the page links holds the catalog', () => {
  const html = readFileSync(join(dist, 'index.html'), 'utf8')
  const linked = [...html.matchAll(/(?:src|href)="(\/assets\/[^"]+\.js)"/g)].map((m) => m[1])
  expect(linked.length).toBeGreaterThan(1)
  const holding = linked.filter((p) => readFileSync(join(dist, p), 'utf8').includes(APPS_CATALOG_ID))
  expect(holding).toEqual([])
})
