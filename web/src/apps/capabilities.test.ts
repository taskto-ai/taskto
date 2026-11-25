// @vitest-environment jsdom


import { existsSync, readFileSync, statSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { expect, test } from 'vitest'
import { COMPONENTS, catalogCapabilities, FUNCTIONS } from './capabilities'
import { appsCatalog } from './catalog'

// capabilities.ts says by name what the catalog holds, so that asking whether a release can be drawn
// doesn't load the renderer. The names must be the catalog's own.
test('the names are the catalog’s components and functions', () => {
  expect([...COMPONENTS].sort()).toEqual([...appsCatalog.components.keys()].sort())
  expect([...FUNCTIONS].sort()).toEqual([...appsCatalog.functions.keys()].sort())
  for (const name of appsCatalog.components.keys()) expect(catalogCapabilities.has(`component.${name}`)).toBe(true)
})

// The modules the app's entry imports, followed through their static imports (not `import type`,
// not import()): what every page loads before it shows anything.
function reachedFromEntry(): Map<string, string | null> {
  const src = resolve(__dirname, '..')
  const file = (from: string, spec: string) => {
    if (!spec.startsWith('.')) return spec
    const base = resolve(dirname(from), spec)
    for (const ext of ['', '.tsx', '.ts', '/index.tsx', '/index.ts'])
      if (existsSync(base + ext) && statSync(base + ext).isFile()) return base + ext
    return base
  }
  const entry = join(src, 'main.tsx')
  const from = new Map<string, string | null>([[entry, null]])
  const queue = [entry]
  while (queue.length > 0) {
    const f = queue.shift() as string
    if (!f.startsWith('/') || !/\.tsx?$/.test(f) || !existsSync(f)) continue
    const code = readFileSync(f, 'utf8')
    for (const m of code.matchAll(/^\s*(?:import|export)\s+(?!type\b)(?:[^'"]*?\s+from\s+)?['"]([^'"]+)['"]/gm)) {
      const dep = file(f, m[1])
      if (!from.has(dep)) {
        from.set(dep, f)
        queue.push(dep)
      }
    }
  }
  for (const [k, v] of from) from.set(k, v && relative(src, v))
  return from
}

test('the renderer is loaded only when an app is drawn, never by the entry', () => {
  const reached = reachedFromEntry()
  // The proof the walk works: the entry reaches the shell and the offline download.
  expect([...reached.keys()].some((f) => f.endsWith('offline/download.ts'))).toBe(true)
  const renderer = [...reached.keys()].filter(
    (k) => /^@a2ui\/|^zod$|^redos-detector$/.test(k) || k.endsWith('apps/catalog.tsx'),
  )
  expect(renderer.map((k) => `${k} (imported by ${reached.get(k)})`)).toEqual([])
})
