// The agent browser's hand-over end to end, with a real model (docs/agent-browser.md).
//
// The Compose stack in container mode with the browser on, built from this checkout, and a
// fake site with a login (fakelogin.mjs) in the CLI worker. The owner's personal provider
// account is an OpenAI-compatible gateway. In a real Chromium (Playwright), the "user" asks
// the agent for the access code on the site's dashboard, which only shows after signing in.
// The test passes when the model opens the site, calls browser_help instead of trying to
// sign in, the user takes over in the live view (noVNC), signs in with keys and clicks sent
// through it, presses Done, and the model carries on to read the code.
//
// It builds the images, runs Docker and pays for model calls, so it runs only with
// TASKTO_HANDOVER_E2E=1, never in CI: `make browser-handover-e2e`. It needs:
//
//   - TASKTO_HANDOVER_E2E_BASE_URL: the gateway's base URL, up to /v1;
//   - TASKTO_HANDOVER_E2E_KEY_FILE: a file with its API key. The key goes from the file to
//     taskto's API (sealed there, as any account's key), never to a log or the command line;
//   - TASKTO_HANDOVER_E2E_MODEL: a model that calls tools (default gpt-oss-120b).
//
// Optional:
//
//   - TASKTO_HANDOVER_E2E_RELAY=1: when Docker's VM can't reach the gateway (Docker in a
//     VM, such as Docker Desktop, on a machine whose gateway is on its LAN), relay TCP to it from this machine:
//     the server container resolves the gateway's name to the Docker host, and TLS still ends
//     at the gateway, checked against its own name. The relay listens on 127.0.0.1 only, which
//     VM-based Docker setups forward host-gateway to; it is never open to the LAN. On Linux,
//     where the containers reach the LAN themselves, it isn't needed.
//   - TASKTO_HANDOVER_E2E_OUT: a directory for screenshots of each step.
//   - TASKTO_HANDOVER_E2E_KEEP_IMAGES=1: keep the built images (taskto-handover-e2e/…) for
//     the next run. The containers, volumes and network always go.

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
// The fake site's account: the person's, which the model never sees.
const site = { user: 'alice', password: `pw-${randomBytes(6).toString('hex')}` }
const code = randomBytes(4).toString('hex').toUpperCase()
const siteURL = 'http://127.0.0.1:8000/'
const minute = 60_000

let dir
let relay
// The relay's open connections, closed with it.
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

  // The relay, if asked: this machine's port to the gateway's, byte for byte.
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

  // The fake site runs on the server's image (it has Node), in the worker's network namespace.
  // The networks get a subnet Docker picks and a bridge name of their own, so the stack runs
  // beside another taskto's.
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
      // The worker's bridge has a fixed name, which another stack on this Docker host may hold.
      worker: { driver_opts: { 'com.docker.network.bridge.name': 'tt-e2e-worker' } },
    },
  }
  // JSON is YAML; compose's !reset drops compose.yaml's fixed subnet.
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
    // Docker picks a free port on 127.0.0.1.
    TASKTO_PORT: '0',
  }
  await compose(['build', 'taskto', 'cli-worker'], 30 * minute)
  up = true
  await compose(['up', '-d'], 5 * minute)
  const published = (await compose(['port', 'taskto', '8080'])).trim().split('\n')[0]
  base = `http://localhost:${published.split(':').at(-1)}`

  // The setup token is in the server's log.
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

  // The owner's own account on the gateway, and a conversation on its model.
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

    // The user's request, in the chat box. The model is told nothing about the hand-over.
    const box = page.getByRole('textbox', { name: 'Message', exact: true })
    await box.fill(`Open ${siteURL} in my personal browser and tell me the access code on its dashboard.`)
    await box.press('Enter')

    // The model calls browser_help: the card shows. It never tried to sign in itself.
    const card = page.getByRole('region', { name: 'The agent needs you in the browser' })
    // The card comes before the model's last words, so a turn that ended without it never asked.
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

    // Take over: the live view, under the user's control.
    await card.getByRole('button', { name: 'Take over' }).click()
    // The live view under the user's control is named so; its status line goes once it shows.
    const live = page.getByRole('img', { name: 'The browser, under your control' })
    await live.waitFor({ timeout: minute })
    const canvas = live.locator('canvas')
    await canvas.waitFor({ timeout: minute })
    await page
      .getByRole('region', { name: 'Your browser' })
      .getByRole('status')
      .waitFor({ state: 'detached', timeout: minute })
    // The first frames.
    await page.waitForTimeout(2000)
    await shot('2-live')

    // Sign in through the live view: one click, then only keys, as a person would. The click
    // goes to the middle of the screen's upper half, which lands in the username input
    // (fakelogin.mjs: 90% wide, 4–38% of the page's height, under Chromium's toolbar) for any
    // toolbar up to a quarter of the 1280x800 screen. Tab, not a second click, finds the password.
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

    // Done: the agent carries on, reads the dashboard and answers with the code.
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
    // One sign-in, the user's.
    assert.deepEqual(await logins(), ['ok=true'])
  } catch (e) {
    await shot('fail').catch(() => {})
    throw e
  } finally {
    await browser.close()
  }
})

// call is the owner's request to taskto's API.
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

// compose runs docker compose on the test's stack and returns its output.
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

// logins is the fake site's login attempts, as it logged them.
async function logins() {
  const logs = await compose(['logs', '--no-log-prefix', 'fakelogin'])
  return [...logs.matchAll(/^login attempt (ok=\w+)$/gm)].map((m) => m[1])
}

// transcript is the conversation so far, for a failure's message.
async function transcript() {
  try {
    const { messages } = await call('GET', `/api/workspaces/${ws}/conversations/${conversation.id}/messages`)
    const errors = await providerErrors()
    return [...messages.map((m) => `${m.role}: ${m.text}`), ...errors.map((e) => `error shown: ${e}`)].join('\n')
  } catch (e) {
    return `(no transcript: ${e.message})`
  }
}

// providerErrors is the model calls' errors the conversation shows, such as a gateway's 429.
async function providerErrors() {
  return page && !page.isClosed() ? page.getByRole('article').getByRole('alert').allTextContents() : []
}

// until waits for check. A model call that failed and wasn't followed by anything for a minute
// (a gateway out of quota, say) ends the wait early, with the error.
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
