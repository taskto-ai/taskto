/// <reference types="vitest/config" />
import { cpSync, createReadStream, existsSync, mkdirSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, join, normalize, sep } from 'node:path'
import react from '@vitejs/plugin-react'
import { defineConfig, type Plugin } from 'vite'
import { notices } from './pwa/notices.ts'
import { pwa } from './pwa/plugin.ts'

// noVNC (MPL-2.0) draws the agent browser's live view (docs/agent-browser.md). Its files are
// shipped as they are, a set of their own beside the app, with its licence: copied unmodified
// from the npm package to /vendor/novnc/ and loaded from there at run time, never bundled
// into our code.
const NOVNC_PATH = '/vendor/novnc/'
// The package exports only core/rfb.js: its root is two levels up.
const novncRoot = dirname(dirname(createRequire(import.meta.url).resolve('@novnc/novnc')))
const NOVNC_FILES = ['core', 'vendor', 'LICENSE.txt', 'AUTHORS', 'package.json', 'docs/LICENSE.MPL-2.0']

function novncReadme(): string {
  const { version } = JSON.parse(readFileSync(join(novncRoot, 'package.json'), 'utf8')) as { version: string }
  return `noVNC ${version}, unmodified, from the npm package @novnc/novnc.
Its core is licensed under the MPL 2.0 (LICENSE.txt, docs/LICENSE.MPL-2.0); vendor/pako under MIT.
Source: https://github.com/novnc/noVNC/tree/v${version}
TaskTo.AI loads core/rfb.js for the agent browser's live view.
`
}

function novnc(): Plugin {
  let outDir = 'dist'
  return {
    name: 'taskto-novnc',
    configResolved(config) {
      outDir = join(config.root, config.build.outDir)
    },
    // In development, the same files from node_modules.
    configureServer(server) {
      server.middlewares.use(NOVNC_PATH, (req, res, next) => {
        let rel: string
        try {
          rel = normalize(decodeURIComponent((req.url ?? '/').split('?')[0])).replace(/^[/\\]+/, '')
        } catch {
          return next() // malformed percent-encoding: not one of its files
        }
        // Only the files a build copies, and only inside the package.
        const shipped = NOVNC_FILES.some((f) => rel === normalize(f) || rel.startsWith(normalize(f) + sep))
        const file = join(novncRoot, rel)
        if (!shipped || !file.startsWith(novncRoot + sep) || !existsSync(file) || !statSync(file).isFile())
          return next()
        res.setHeader('Content-Type', file.endsWith('.js') ? 'text/javascript' : 'text/plain; charset=utf-8')
        createReadStream(file).pipe(res)
      })
    },
    closeBundle() {
      const dest = join(outDir, NOVNC_PATH)
      for (const f of NOVNC_FILES) {
        mkdirSync(dirname(join(dest, f)), { recursive: true })
        cpSync(join(novncRoot, f), join(dest, f), { recursive: true })
      }
      writeFileSync(join(dest, 'README.txt'), novncReadme())
    },
  }
}

export default defineConfig({
  plugins: [react(), novnc(), notices(import.meta.dirname), pwa(join(import.meta.dirname, 'pwa/sw.template.js'))],
  // A test may wait several times, each up to src/test/setup.ts's 4 s: 10 s for the whole test
  // lets a wait that never ends fail with Testing Library's own message, not vitest's timeout.
  test: { setupFiles: ['src/test/setup.ts'], testTimeout: 10_000 },
  server: {
    // The Go server (cmd/taskto) answers the API during development, the live view's
    // WebSocket included.
    proxy: { '/healthz': 'http://127.0.0.1:8080', '/api': { target: 'http://127.0.0.1:8080', ws: true } },
  },
})
