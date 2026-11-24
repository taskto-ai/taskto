// What the web app's end to end tests share (`make web-e2e`, docs/web-e2e.md): the built server
// on a fresh data directory, a real Chromium (Playwright), and the traces and screenshots a
// failing scenario leaves behind.
//
// Every scenario starts a server of its own, so scenarios never see one another's data and can
// run in any order. There is no agent runtime and no model: nothing here reaches the network,
// and the browser is the only thing downloaded (`playwright install chromium`).

import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { mkdirSync, mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'

const repo = fileURLToPath(new URL('../..', import.meta.url))

// Where a failing scenario's trace and screenshot go (CI keeps this folder as an artifact).
export const resultsDir = process.env.TASKTO_WEB_E2E_RESULTS || join(repo, 'web/e2e-results')

export const owner = {
  email: 'owner@example.com',
  name: 'Owner',
  password: 'correct horse battery staple',
  workspace: 'Home',
}

// Starts bin/taskto (TASKTO_WEB_E2E_BIN names another) on a fresh data directory and waits until
// it listens. The setup token is in the server's log while no account exists.
export async function startServer() {
  const dir = mkdtempSync(join(tmpdir(), 'tt-web-e2e-'))
  const bin = process.env.TASKTO_WEB_E2E_BIN || join(repo, 'bin/taskto')
  const server = spawn(bin, ['-addr', '127.0.0.1:0', '-data-dir', join(dir, 'data'), '-insecure-cookies'], {
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
    // No bin/taskto: `make web-e2e` builds it first.
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
  // Until the server is up the caller has nothing to stop: a failed start cleans up here.
  try {
    const { addr, built, token } = await listening
    assert.equal(built, 'true', 'the web app isn’t built into the binary: run make web-e2e (it builds first)')
    return { base: `http://${addr}`, token, logs: () => logs, stop }
  } catch (err) {
    await stop()
    throw err
  }
}

// A scenario: a fresh server, a browser context with its trace running, and `fn({ page, context,
// server, base })`. When it fails, the trace (a zip for `playwright show-trace`) and a screenshot
// of every open page are written to resultsDir, then the failure goes on.
export function scenario(name, fn, { viewport = { width: 1280, height: 900 } } = {}) {
  test(name, { timeout: 120_000 }, async () => {
    const server = await startServer()
    try {
      const browser = await chromium.launch()
      try {
        const context = await browser.newContext({ baseURL: server.base, viewport })
        context.setDefaultTimeout(15_000)
        await context.tracing.start({ screenshots: true, snapshots: true, sources: false })
        const page = await context.newPage()
        const messages = []
        page.on('console', (m) => m.type() === 'error' && messages.push(`console: ${m.text()}`))
        page.on('pageerror', (e) => messages.push(`pageerror: ${e.message}`))
        try {
          await fn({ page, context, server, base: server.base, messages })
          await context.tracing.stop()
        } catch (err) {
          const slug = name.replace(/[^a-z0-9]+/gi, '-').toLowerCase()
          const out = join(resultsDir, slug)
          mkdirSync(out, { recursive: true })
          await context.tracing.stop({ path: join(out, 'trace.zip') }).catch(() => {})
          for (const [i, p] of context.pages().entries()) {
            await p.screenshot({ path: join(out, `page-${i}.png`), fullPage: true }).catch(() => {})
          }
          console.error(
            `${name}: page errors: ${messages.join(' | ') || 'none'}\nserver log (last lines):\n${server.logs().trim().split('\n').slice(-15).join('\n')}`,
          )
          throw err
        }
      } finally {
        await browser.close()
      }
    } finally {
      await server.stop()
    }
  })
}

// First-run setup through the screen: the token from the server's log, then the owner.
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

// The browser's own JSON calls to the API, with its session and the header the CSRF guard wants.
export async function api(context, method, path, body) {
  const res = await context.request.fetch(path, {
    method,
    headers: { 'Content-Type': 'application/json' },
    data: body === undefined ? undefined : JSON.stringify(body),
  })
  assert.ok(res.ok(), `${method} ${path}: ${res.status()} ${await res.text()}`)
  return res.status() === 204 ? null : res.json()
}

// The Expense tracker template, installed from the Apps page into the owner's private space and
// published, through the screens; it ends on the live app. Returns where the app is on the API.
export async function installExpenseTracker({ page, context }) {
  await page.getByRole('link', { name: 'Apps', exact: true }).click()
  await page.getByRole('link', { name: 'Start from a template' }).first().click()
  await page.getByRole('button', { name: 'Use the template Expense tracker' }).click()
  await page.getByRole('button', { name: 'Install as a draft' }).click()
  await page.getByText('This is a draft: nothing is live yet.').waitFor()
  await page.getByRole('button', { name: 'Publish', exact: true }).click()
  await page.getByText('Published as release 1.').waitFor()
  await page.getByRole('link', { name: 'All apps' }).click()
  await page.getByRole('link', { name: 'Launch' }).click()
  await page.getByRole('button', { name: 'New expense' }).waitFor()

  const { spaces } = await api(context, 'GET', '/api/workspaces/1/spaces')
  const space = spaces.find((s) => s.kind === 'private')
  const { apps } = await api(context, 'GET', `/api/workspaces/1/spaces/${space.id}/apps`)
  return { records: `/api/workspaces/1/spaces/${space.id}/apps/${apps[0].id}/records` }
}

// Fills the template's expense form and saves it.
export async function addExpense(page, { what, amount }) {
  await page.getByRole('button', { name: 'New expense' }).click()
  await page.getByRole('textbox', { name: 'What was it for?' }).fill(what)
  await page.getByLabel('Amount').fill(String(amount))
  await page.getByLabel('Date').fill('2026-11-20')
  await page.getByRole('button', { name: 'Food', exact: true }).click()
  await page.getByRole('button', { name: 'Save expense' }).click()
}
