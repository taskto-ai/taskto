import { createHash } from 'node:crypto'
import { readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import { join, relative, sep } from 'node:path'
import type { Plugin } from 'vite'

const SKIP = [
  /^sw\.js$/,
  /\.map$/,
  /^vendor\//,
  /(^|\/)zxing_writer[^/]*\.wasm$/,
  /(^|\/)docx-preview-[^/]*\.js$/,
  /(^|\/)THIRD_PARTY_NOTICES\.txt$/,
]

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
  precache: "['__PRECACHE__']",
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
