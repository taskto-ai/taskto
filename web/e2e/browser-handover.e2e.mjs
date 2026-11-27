import assert from 'node:assert/strict'
import { execFile } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { createServer, connect as tcpConnect } from 'node:net'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, before, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'
import { chromium } from 'playwright'

const run = promisify(execFile)
const enabled = process.env.TASKTO_HANDOVER_E2E === '1'
const repo = fileURLToPath(new URL('../..', import.meta.url))
const model = process.env.TASKTO_HANDOVER_E2E_MODEL || 'gpt-oss-120b'
const out = process.env.TASKTO_HANDOVER_E2E_OUT
const project = 'taskto-handover-e2e'
const prefix = `${project}/`
const email = 'owner@example.com'
const password = 'handover-e2e-password'
const site = { user: 'alice', password: `pw-${randomBytes(6).toString('hex')}` }
const code = randomBytes(4).toString('hex').toUpperCase()
const siteURL = 'http://127.0.0.1:8000/'
const minute = 60_000

let dir
let relay
const relayed = new Set()
let composeArgs
let composeEnv
let base
let cookie
let ws
let conversation
let page
let up = false

before(async () => {
  if (!enabled) return
  const baseURL = process.env.TASKTO_HANDOVER_E2E_BASE_URL
  const keyFile = process.env.TASKTO_HANDOVER_E2E_KEY_FILE
  assert.ok(baseURL && keyFile, 'set TASKTO_HANDOVER_E2E_BASE_URL and TASKTO_HANDOVER_E2E_KEY_FILE')
  const gateway = new URL(baseURL)
  dir = mkdtempSync(join(tmpdir(), 'tt-handover-'))
  if (out) mkdirSync(out, { recursive: true })

  let providerURL = baseURL
  const extraHosts = []
  if (process.env.TASKTO_HANDOVER_E2E_RELAY === '1') {
    const port = Number(gateway.port) || (gateway.protocol === 'https:' ? 443 : 80)
    relay = createServer((c) => {
      const u = tcpConnect(port, gateway.hostname)
      relayed.add(c).add(u)
      c.on('close', () => relayed.delete(c))
      u.on('close', () => relayed.delete(u))
      c.pipe(u).pipe(c)
      c.on('error', () => u.destroy())
      u.on('error', () => c.destroy())
    })
    await new Promise((resolve) => relay.listen(0, '127.0.0.1', resolve))
    const via = new URL(baseURL)
    via.port = String(relay.address().port)
    providerURL = via.toString().replace(/\/$/, '')
    extraHosts.push(`${gateway.hostname}:host-gateway`)
  }

  const override = {
    services: {
      taskto: extraHosts.length ? { extra_hosts: extraHosts } : {},
      fakelogin: {
        profiles: ['cli'],
        image: `${prefix}taskto:dev`,
        pull_policy: 'never',
        network_mode: 'service:cli-worker',
        entrypoint: ['/nodejs/bin/node', '/srv/fakelogin.mjs'],
        environment: { ACCESS_CODE: code, LOGIN_USER: site.user, LOGIN_PASSWORD: site.password },
        volumes: [`${join(repo, 'web/e2e/fakelogin.mjs')}:/srv/fakelogin.mjs:ro`],
        read_only: true,
        cap_drop: ['ALL'],
        security_opt: ['no-new-privileges:true'],
      },
    },
    networks: {
      default: { ipam: 'RESET' },
      worker: { driver_opts: { 'com.docker.network.bridge.name': 'tt-e2e-worker' } },
    },
  }
  const yaml = JSON.stringify(override, null, 2).replace('"ipam": "RESET"', '"ipam": !reset {}')
  writeFileSync(join(dir, 'compose.override.yaml'), yaml)
  composeArgs = ['compose', '-p', project, '-f', join(repo, 'compose.yaml'), '-f', join(dir, 'compose.override.yaml')]
  composeArgs.push('--profile', 'cli')
  composeEnv = {
    ...process.env,
    TASKTO_IMAGE_PREFIX: prefix,
    TASKTO_VERSION: 'dev',
    TASKTO_CLI_WORKERS: 'container',
    TASKTO_BROWSER: 'true',
    TASKTO_PORT: '0',
  }
  await compose(['build', 'taskto', 'cli-worker'], 30 * minute)
  up = true
  await compose(['up', '-d'], 5 * minute)
  const published = (await compose(['port', 'taskto', '8080'])).trim().split('\n')[0]
  base = `http://localhost:${published.split(':').at(-1)}`

  const deadline = Date.now() + 2 * minute
  let token
  for (;;) {
    const logs = await compose(['logs', '--no-log-prefix', 'taskto'])
    token = logs.match(/setup_token=([A-Za-z0-9_-]{43})/)?.[1]
    if (token) break
    assert.ok(Date.now() < deadline, `no setup token in the server's log after two minutes:\n${logs}`)
    await sleep(1000)
  }
  const res = await fetch(`${base}/api/setup`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, name: 'Owner', password, workspace_name: 'Home', setup_token: token }),
  })
  assert.equal(res.status, 201, await res.clone().text())
  cookie = res.headers
    .getSetCookie()
    .map((c) => c.split(';')[0])
    .join('; ')
  ws = (await res.json()).workspaces[0].id

  const browser = await call('GET', '/api/me/browser')
  assert.ok(browser.enabled, `the browser isn't enabled: ${browser.reason}`)
  assert.ok(browser.browsers.personal.available, "the worker has no personal browser's programs")

  const apiKey = readFileSync(keyFile, 'utf8').trim()
  const account = await call('POST', '/api/me/providers', {
    name: 'gateway',
    base_url: providerURL,
    api_key: apiKey,
    models: [{ id: model }],
  })
  const check = await call('POST', `/api/me/providers/${account.id}/test`, {})
  assert.ok(check.ok, `the gateway didn't answer: ${check.error}`)
  conversation = await call('POST', `/api/workspaces/${ws}/conversations`, {
    title: `Hand-over with ${model}`,
    model: { provider: account.provider, model_id: model },
  })
})

after(async () => {
  if (up) {
    await compose(['down', '-v', '--remove-orphans'], 5 * minute).catch((e) => console.error(e.message))
    if (process.env.TASKTO_HANDOVER_E2E_KEEP_IMAGES !== '1') {
      await run('docker', ['image', 'rm', `${prefix}taskto:dev`, `${prefix}taskto-cli-worker:dev`]).catch((e) =>
        console.error(e.message),
      )
    }
  }
  for (const c of relayed) c.destroy()
  relay?.close()
  if (dir) rmSync(dir, { recursive: true, force: true })
})

test('a real model asks for help, the user signs in in the live view, and the agent carries on', {
  skip: !enabled && 'set TASKTO_HANDOVER_E2E=1',
  timeout: 20 * minute,
}, async () => {
  const browser = await chromium.launch()
  page = await browser.newPage({ viewport: { width: 1600, height: 1000 } })
  const shot = async (name) => {
    if (out) await page.screenshot({ path: join(out, `${model}-${name}.png`) })
  }
  try {
    await page.goto(`${base}/`)
    await page.locator('input[type=email]').fill(email)
    await page.locator('input[type=password]').fill(password)
    await page.getByRole('button', { name: 'Log in' }).click()
    await page.waitForURL(/\/w\/\d+\//)
    await page.goto(`${base}/w/${ws}/conversations/${conversation.id}`)

    const box = page.getByRole('textbox', { name: 'Message', exact: true })
    await box.fill(`Open ${siteURL} in my personal browser and tell me the access code on its dashboard.`)
    await box.press('Enter')

    const card = page.getByRole('region', { name: 'The agent needs you in the browser' })
    let ended = 0
    await until(
      async () => {
        if (await card.isVisible()) return true
        const { messages } = await call('GET', `/api/workspaces/${ws}/conversations/${conversation.id}/messages`)
        const last = messages.at(-1)
        ended = last?.role === 'assistant' && last.text.trim() ? ended || Date.now() : 0
        assert.ok(!ended || Date.now() - ended < 5000, `the turn ended without browser_help\n${await transcript()}`)
        return false
      },
      6 * minute,
      'no help card',
    ).catch(async (e) => {
      await shot('fail-no-help')
      throw e
    })
    assert.deepEqual(await logins(), [], 'the agent tried to sign in')
    const tab = (await call('GET', '/api/me/browser')).browsers.personal.tabs.find((t) => t.url.startsWith(siteURL))
    assert.ok(tab?.agent, "the agent's tab on the site isn't open")
    await shot('1-help')

    await card.getByRole('button', { name: 'Take over' }).click()
    const live = page.getByRole('img', { name: 'The browser, under your control' })
    await live.waitFor({ timeout: minute })
    const canvas = live.locator('canvas')
    await canvas.waitFor({ timeout: minute })
    await page
      .getByRole('region', { name: 'Your browser' })
      .getByRole('status')
      .waitFor({ state: 'detached', timeout: minute })
    await page.waitForTimeout(2000)
    await shot('2-live')

    const b = await canvas.boundingBox()
    await page.mouse.click(b.x + b.width / 2, b.y + b.height * 0.3)
    await page.keyboard.type(site.user, { delay: 30 })
    await page.keyboard.press('Tab')
    await page.keyboard.type(site.password, { delay: 30 })
    await page.keyboard.press('Enter')
    await until(
      async () =>
        (await call('GET', '/api/me/browser')).browsers.personal.tabs.some((t) => t.url.endsWith('/dashboard')),
      minute,
      'the site never reached its dashboard after signing in through the live view',
    )
    assert.deepEqual(await logins(), ['ok=true'])
    await page.waitForTimeout(1000)
    await shot('3-signed-in')

    await card.getByRole('button', { name: 'Done' }).click()
    await card.waitFor({ state: 'detached', timeout: minute })
    let answer
    await until(
      async () => {
        const { messages } = await call('GET', `/api/workspaces/${ws}/conversations/${conversation.id}/messages`)
        const done = messages.findLastIndex((m) => m.role === 'user' && m.text.startsWith('Done:'))
        answer = messages.slice(done + 1).find((m) => m.role === 'assistant' && m.text.includes(code))
        return done >= 0 && answer
      },
      6 * minute,
      'the agent never answered with the access code after Done',
    )
    console.log(`${model} answered: ${answer.text}`)
    await page.getByText(code).first().waitFor({ timeout: minute })
    await shot('4-answer')
    assert.deepEqual(await logins(), ['ok=true'])
  } catch (e) {
    await shot('fail').catch(() => {})
    throw e
  } finally {
    await browser.close()
  }
})

async function call(method, path, body) {
  const res = await fetch(`${base}${path}`, {
    method,
    headers: { Cookie: cookie, ...(body ? { 'Content-Type': 'application/json' } : {}) },
    body: body && JSON.stringify(body),
    signal: AbortSignal.timeout(minute),
  })
  assert.ok(res.ok, `${method} ${path}: ${res.status} ${await res.clone().text()}`)
  return res.status === 204 ? null : res.json()
}

async function compose(args, timeout = minute) {
  try {
    const { stdout, stderr } = await run('docker', [...composeArgs, ...args], {
      env: composeEnv,
      timeout,
      maxBuffer: 64 << 20,
    })
    return stdout + stderr
  } catch (e) {
    throw new Error(`docker compose ${args.join(' ')}: ${e.message}\n${e.stderr ?? ''}`.slice(0, 20_000))
  }
}

async function logins() {
  const logs = await compose(['logs', '--no-log-prefix', 'fakelogin'])
  return [...logs.matchAll(/^login attempt (ok=\w+)$/gm)].map((m) => m[1])
}

async function transcript() {
  try {
    const { messages } = await call('GET', `/api/workspaces/${ws}/conversations/${conversation.id}/messages`)
    const errors = await providerErrors()
    return [...messages.map((m) => `${m.role}: ${m.text}`), ...errors.map((e) => `error shown: ${e}`)].join('\n')
  } catch (e) {
    return `(no transcript: ${e.message})`
  }
}

async function providerErrors() {
  return page && !page.isClosed() ? page.getByRole('article').getByRole('alert').allTextContents() : []
}

async function until(check, timeout, what) {
  const deadline = Date.now() + timeout
  let errors = 0
  let since = Date.now()
  while (!(await check())) {
    const n = (await providerErrors()).length
    if (n !== errors) [errors, since] = [n, Date.now()]
    if (Date.now() > deadline || (errors && Date.now() - since > minute)) {
      throw new Error(`${what}\n${await transcript()}`)
    }
    await sleep(1000)
  }
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}
