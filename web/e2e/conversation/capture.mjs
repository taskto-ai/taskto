// Captures the conversations page from harness.tsx (a fake API and a replayed stream, no
// server, no agent runtime) in Playwright's Chromium, at the sizes and themes of make screens:
//
//   node web/e2e/conversation/capture.mjs [out-dir] [scenario,…]
//
// Writes <out>/<size>-<theme>/conversation-<scenario>.png (default out: web/screens/harness).
// Development only; CI never runs it.

import { mkdirSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const web = resolve(dirname(fileURLToPath(import.meta.url)), '../..')
const out = resolve(process.argv[2] ?? join(web, 'screens/harness'))
const scenarios = (process.argv[3] ?? 'chat,running,cli,list,empty,failed').split(',')
const sizes = { desktop: [1440, 900], tablet: [820, 1100], phone: [390, 844] }
const themes = ['light', 'dark']

const { createServer } = await import('vite')
const server = await createServer({
  root: web,
  configFile: join(web, 'vite.config.ts'),
  server: { port: 0, host: '127.0.0.1' },
  logLevel: 'error',
})
await server.listen()
const base = server.resolvedUrls.local[0]
const { chromium } = await import('playwright')
const browser = await chromium.launch()
let failed = 0
try {
  for (const [size, [width, height]] of Object.entries(sizes)) {
    for (const theme of themes) {
      const page = await browser.newPage({ viewport: { width, height }, colorScheme: theme })
      page.on('pageerror', (e) => console.error(`${size}-${theme}: ${e.message}`))
      for (const s of scenarios) {
        try {
          await page.goto(`${base}e2e/conversation/index.html?s=${s}`)
          await page.waitForSelector('[aria-label="Conversations"]', { state: 'attached', timeout: 15000 })
          // The stream's view, the inline app and the fonts.
          await page.waitForTimeout(s === 'chat' ? 2500 : 1200)
          await page.evaluate(() => document.fonts.ready)
          const file = join(out, `${size}-${theme}`, `conversation-${s}.png`)
          mkdirSync(dirname(file), { recursive: true })
          await page.screenshot({ path: file })
          console.log(file)
        } catch (e) {
          failed++
          console.error(`${size}-${theme}/${s}: ${e.message}`)
        }
      }
      await page.close()
    }
  }
} finally {
  await browser.close()
  await server.close()
}
process.exit(failed ? 1 : 0)
