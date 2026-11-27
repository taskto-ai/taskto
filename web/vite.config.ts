/// <reference types="vitest/config" />
import { cpSync, createReadStream, existsSync, mkdirSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, join, normalize, sep } from 'node:path'
import react from '@vitejs/plugin-react'
import { defineConfig, type Plugin } from 'vite'
import { notices } from './pwa/notices.ts'
import { pwa } from './pwa/plugin.ts'

const NOVNC_PATH = '/vendor/novnc/'
const novncRoot = dirname(dirname(createRequire(import.meta.url).resolve('@novnc/novnc')))
const NOVNC_FILES = ['core', 'vendor', 'LICENSE.txt', 'AUTHORS', 'package.json', 'docs/LICENSE.MPL-2.0']

function novncReadme(): string {
  const { version } = JSON.parse(readFileSync(join(novncRoot, 'package.json'), 'utf8')) as { version: string }
  return `noVNC ${version}, unmodified, from the npm package @novnc/novnc.
Its core is licensed under the MPL 2.0 (LICENSE.txt, docs/LICENSE.MPL-2.0); vendor/pako under MIT.
Source: https://github.com/novnc/noVNC/tree/v${version}
taskto.ai loads core/rfb.js for the agent browser's live view.
`
}

function novnc(): Plugin {
  let outDir = 'dist'
  return {
    name: 'taskto-novnc',
    configResolved(config) {
      outDir = join(config.root, config.build.outDir)
    },
    configureServer(server) {
      server.middlewares.use(NOVNC_PATH, (req, res, next) => {
        let rel: string
        try {
          rel = normalize(decodeURIComponent((req.url ?? '/').split('?')[0])).replace(/^[/\\]+/, '')
        } catch {
          return next()
        }
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
  test: { setupFiles: ['src/test/setup.ts'], testTimeout: 10_000 },
  server: {
    proxy: { '/healthz': 'http://127.0.0.1:8080', '/api': { target: 'http://127.0.0.1:8080', ws: true } },
  },
})
