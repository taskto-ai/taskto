import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { mkdirSync, mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'

const repo = fileURLToPath(new URL('../..', import.meta.url))

export const resultsDir = process.env.TASKTO_WEB_E2E_RESULTS || join(repo, 'web/e2e-results')

export const owner = {
  email: 'owner@example.com',
  name: 'Owner',
  password: 'correct horse battery staple',
  workspace: 'Home',
}

export async function startServer() {
  const dir = mkdtempSync(join(tmpdir(), 'tt-web-e2e-'))
  const bin = process.env.TASKTO_WEB_E2E_BIN || join(repo, 'bin/taskto')
  const env = Object.fromEntries(Object.entries(process.env).filter(([name]) => !name.startsWith('TASKTO_')))
  const server = spawn(bin, ['-addr', '127.0.0.1:0', '-data-dir', join(dir, 'data'), '-insecure-cookies'], {
    env,
    stdio: ['ignore', 'ignore', 'pipe'],
  })
  let logs = ''
  const listening = new Promise((resolve, reject) => {
    const giveUp = setTimeout(() => reject(new Error(`the server didn't start in a minute:\n${logs}`)), 60_000)
    server.stderr.on('data', (b) => {
      logs += b
      const addr = logs.match(/taskto listening addr=(\S+).*web_app_built=(\w+)/)
      const token = logs.match(/setup_token=([A-Za-z0-9_-]{43})/)
      if (addr && token) {
        clearTimeout(giveUp)
        resolve({ addr: addr[1], built: addr[2], token: token[1] })
      }
    })
    const fail = (e) => {
      clearTimeout(giveUp)
      reject(e)
    }
    server.on('error', (e) => fail(new Error(`the server didn't start: ${e.message}`)))
    server.on('exit', (code) => fail(new Error(`the server exited (${code}):\n${logs}`)))
  })
  const stop = async () => {
    server.removeAllListeners('exit')
    if (server.exitCode === null && server.signalCode === null) {
      await new Promise((resolve) => {
        const kill = setTimeout(() => server.kill('SIGKILL'), 10_000)
        server.once('exit', () => {
          clearTimeout(kill)
          resolve()
        })
        server.kill('SIGTERM')
      })
    }
    rmSync(dir, { recursive: true, force: true })
  }
  try {
    const { addr, built, token } = await listening
    assert.equal(built, 'true', 'the web app isn’t built into the binary: run make web-e2e (it builds first)')
    return { base: `http://${addr}`, token, logs: () => logs, stop }
  } catch (err) {
    await stop()
    throw err
  }
}

export function scenario(name, fn, { viewport = { width: 1280, height: 900 } } = {}) {
  test(name, { timeout: 120_000 }, async (t) => {
    let browser
    let context
    let server
    let passed = false
    const messages = []
    t.after(async () => {
      try {
        if (!passed && context) {
          const slug = name.replace(/[^a-z0-9]+/gi, '-').toLowerCase()
          const out = join(resultsDir, slug)
          mkdirSync(out, { recursive: true })
          await context.tracing.stop({ path: join(out, 'trace.zip') }).catch(() => {})
          for (const [i, p] of context.pages().entries()) {
            await p.screenshot({ path: join(out, `page-${i}.png`), fullPage: true, timeout: 5_000 }).catch(() => {})
          }
          console.error(
            `${name}: page errors: ${messages.join(' | ') || 'none'}\nserver log (last lines):\n${server.logs().trim().split('\n').slice(-15).join('\n')}`,
          )
        }
      } finally {
        try {
          await browser?.close()
        } finally {
          await server?.stop()
        }
      }
    })
    server = await startServer()
    browser = await chromium.launch()
    context = await browser.newContext({ baseURL: server.base, viewport })
    context.setDefaultTimeout(15_000)
    await context.tracing.start({ screenshots: true, snapshots: true, sources: false })
    const page = await context.newPage()
    page.on('console', (m) => m.type() === 'error' && messages.push(`console: ${m.text()}`))
    page.on('pageerror', (e) => messages.push(`pageerror: ${e.message}`))
    await fn({ page, context, server, base: server.base, messages })
    await context.tracing.stop()
    passed = true
  })
}

export async function setUp({ page, server }, who = owner) {
  await page.goto('/')
  await page.getByRole('heading', { name: 'Set up this server' }).waitFor()
  await page.getByLabel('Setup token').fill(server.token)
  await page.getByLabel('Your name').fill(who.name)
  await page.getByLabel('Email').fill(who.email)
  await page.getByLabel('Password', { exact: true }).fill(who.password)
  await page.getByLabel('Workspace name').fill(who.workspace)
  await page.getByRole('button', { name: 'Create the owner' }).click()
  await page.getByRole('navigation', { name: 'Sections' }).waitFor()
}

export async function api(context, method, path, body) {
  const res = await context.request.fetch(path, {
    method,
    headers: { 'Content-Type': 'application/json' },
    data: body === undefined ? undefined : JSON.stringify(body),
  })
  assert.ok(res.ok(), `${method} ${path}: ${res.status()} ${await res.text()}`)
  return res.status() === 204 ? null : res.json()
}

export async function installExpenseTracker({ page, context }) {
  await page.getByRole('link', { name: 'Apps', exact: true }).click()
  await page.getByRole('link', { name: 'Start from a template' }).first().click()
  await page.getByRole('button', { name: 'Use the template Expense tracker' }).click()
  await page.getByRole('button', { name: 'Install as a draft' }).click()
  await page.getByText('This is a draft: nothing is live yet.').waitFor()
  await page.getByRole('button', { name: 'Publish', exact: true }).click()
  await page.getByText('Published as release 1.').waitFor()
  await page.getByRole('link', { name: 'All apps' }).click()
  await page.getByRole('link', { name: 'Open Expense tracker' }).click()
  await page.getByRole('button', { name: 'New expense' }).waitFor()

  const { spaces } = await api(context, 'GET', '/api/workspaces/1/spaces')
  const space = spaces.find((s) => s.kind === 'private')
  const { apps } = await api(context, 'GET', `/api/workspaces/1/spaces/${space.id}/apps`)
  return { records: `/api/workspaces/1/spaces/${space.id}/apps/${apps[0].id}/records` }
}

export async function addExpense(page, { what, amount }) {
  await page.getByRole('button', { name: 'New expense' }).click()
  await page.getByRole('textbox', { name: 'What was it for?' }).fill(what)
  await page.getByLabel('Amount').fill(String(amount))
  await page.getByLabel('Date').fill('2026-11-20')
  await page.getByRole('button', { name: 'Food', exact: true }).click()
  await page.getByRole('button', { name: 'Save expense' }).click()
}
