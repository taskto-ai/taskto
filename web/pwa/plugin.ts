// The build's part of the PWA (docs/pwa.md): after Vite has written the app, lists the static
// files the service worker precaches and writes pwa/sw.template.js to /sw.js with that list
// and a version made from their content. A file's name carries a hash of its content under
// assets/, and the version is a hash of all of them, so the worker's own bytes change exactly
// when the app does, and the browser installs it as an update.

import { createHash } from 'node:crypto'
import { readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import { join, relative, sep } from 'node:path'
import type { Plugin } from 'vite'

// Files the worker leaves out: itself, source maps, the vendored noVNC (the agent
// browser's live view, which needs the network anyway, and is large), the QR writer's .wasm
// (drawing the code that turns two-factor login on needs the server as well) and the licence
// notices, which are there to be read, not to run offline.
const SKIP = [/^sw\.js$/, /\.map$/, /^vendor\//, /(^|\/)zxing_writer[^/]*\.wasm$/, /(^|\/)THIRD_PARTY_NOTICES\.txt$/]

// The URLs to precache, from the build's files (relative, with "/"): the page under the one
// name the worker serves it from, then everything else. Sorted, so the list is stable.
export function precacheEntries(files: string[]): string[] {
  const urls = files
    .filter((f) => !SKIP.some((re) => re.test(f)))
    .map((f) => `/${f}`)
    .sort()
  if (!urls.includes('/index.html')) throw new Error('pwa: the build has no index.html')
  return urls
}

export const PLACEHOLDER = {
  version: "'__VERSION__'",
  precache: '/* __PRECACHE__ */ []',
}

export function renderServiceWorker(template: string, entries: string[], version: string): string {
  for (const marker of Object.values(PLACEHOLDER)) {
    if (!template.includes(marker)) throw new Error(`pwa: the template lacks ${marker}`)
  }
  return template
    .replace(PLACEHOLDER.version, JSON.stringify(version))
    .replace(PLACEHOLDER.precache, JSON.stringify(entries, null, 2))
}

function walk(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name)
    return statSync(path).isDirectory() ? walk(path) : [path]
  })
}

export function pwa(template: string): Plugin {
  let outDir = 'dist'
  return {
    name: 'taskto-pwa',
    apply: 'build',
    configResolved(config) {
      outDir = join(config.root, config.build.outDir)
    },
    // After every other plugin's closeBundle (noVNC copies its files there), so the list
    // is of what the build really holds.
    closeBundle: {
      order: 'post',
      handler() {
        const files = walk(outDir).map((p) => relative(outDir, p).split(sep).join('/'))
        const entries = precacheEntries(files)
        const hash = createHash('sha256')
        for (const e of entries)
          hash
            .update(e)
            .update('\0')
            .update(readFileSync(join(outDir, e.slice(1))))
            .update('\0')
        const version = hash.digest('hex').slice(0, 16)
        writeFileSync(join(outDir, 'sw.js'), renderServiceWorker(readFileSync(template, 'utf8'), entries, version))
      },
    },
  }
}
