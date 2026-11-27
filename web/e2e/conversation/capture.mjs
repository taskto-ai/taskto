import assert from 'node:assert/strict'
import { mkdirSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const web = resolve(dirname(fileURLToPath(import.meta.url)), '../..')
const out = resolve(process.argv[2] ?? join(web, 'screens/harness'))
const scenarios = (process.argv[3] ?? 'chat,running,cli,cli-allow,browser,long,flaky,list,empty,failed').split(',')
const sizes = { desktop: [1440, 900], tablet: [820, 1100], phone: [390, 844], short: [390, 480] }
const themes = ['light', 'dark']
const listOnly = new Set(['list', 'empty'])

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
const fail = (what, e) => {
  failed++
  console.error(`${what}: ${e.message}`)
}

async function checkLayout(page, s) {
  const transcript = page.locator('.ui-chat-transcript')
  if ((await transcript.count()) === 1 && (await transcript.isVisible())) {
    await transcript.evaluate((el) => {
      el.scrollTop = el.scrollHeight
    })
    await transcript.hover()
    await page.mouse.wheel(0, 3000)
    await page.waitForTimeout(200)
  }
  const doc = await page.evaluate(() => ({
    y: window.scrollY,
    height: document.documentElement.scrollHeight,
    window: window.innerHeight,
  }))
  assert.equal(doc.y, 0, `the window scrolled by ${doc.y}px`)
  assert.ok(doc.height <= doc.window, `the page is ${doc.height}px tall in a ${doc.window}px window`)
  if (listOnly.has(s)) return
  const send = page.getByRole('button', { name: /^(Send|Steer)$/ })
  const box = await send.boundingBox()
  assert.ok(box && box.y + box.height <= doc.window, `Send ends at ${box && box.y + box.height}px, below the window`)
}

async function checkLocators(page) {
  await page.goto(`${base}e2e/conversation/index.html?s=chat`)
  await page.getByRole('heading', { name: 'Plan the launch week' }).waitFor()
  await page.waitForTimeout(1000)
  assert.equal(await page.getByRole('textbox', { name: 'Message', exact: true }).count(), 1, 'the message box')
  const alerts = page.getByRole('region', { name: 'Conversation' }).getByRole('alert')
  assert.deepEqual(await alerts.allTextContents(), ['The provider is rate limited. Try again in a minute.'])
  assert.deepEqual(await page.getByRole('article').getByRole('alert').allTextContents(), [
    'The provider is rate limited. Try again in a minute.',
  ])

  await page.goto(`${base}e2e/conversation/index.html?s=browser`)
  const card = page.getByRole('region', { name: 'The agent needs you in the browser' })
  await card.waitFor()
  assert.equal(await page.getByRole('textbox', { name: 'Message', exact: true }).count(), 1, 'the message box')
  await card.getByRole('button', { name: 'Take over' }).click()
  const live = page.getByRole('img', { name: 'The browser, under your control' })
  await live.waitFor({ timeout: 5000 })
  assert.equal(await live.count(), 1, 'the live view under control')
  const status = page.getByRole('region', { name: 'Your browser' }).getByRole('status')
  await status.first().waitFor({ timeout: 5000 })
  assert.equal(await status.count(), 1, "the live view's status line")
  console.log('locators: each finds exactly what it should')
}

try {
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } })
  try {
    await checkLocators(page)
  } catch (e) {
    fail('locators', e)
  }
  await page.close()

  for (const [size, [width, height]] of Object.entries(sizes)) {
    for (const theme of themes) {
      const page = await browser.newPage({ viewport: { width, height }, colorScheme: theme })
      page.on('pageerror', (e) => console.error(`${size}-${theme}: ${e.message}`))
      for (const s of scenarios) {
        try {
          await page.goto(`${base}e2e/conversation/index.html?s=${s}`)
          await page.waitForSelector('[aria-label="Conversations"]', { state: 'attached', timeout: 15000 })
          await page.waitForTimeout(s === 'chat' ? 2500 : 1500)
          await page.evaluate(() => document.fonts.ready)
          const file = join(out, `${size}-${theme}`, `conversation-${s}.png`)
          mkdirSync(dirname(file), { recursive: true })
          await page.screenshot({ path: file })
          await checkLayout(page, s)
          console.log(file)
        } catch (e) {
          fail(`${size}-${theme}/${s}`, e)
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
