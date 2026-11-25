// Dictation end to end (docs/dictation.md): the server binary with the web app built in and
// the real local model, and a real Chromium (Playwright) whose microphone is a WAV file. For
// each clip it signs in, presses the chat box's microphone, records the whole clip, stops,
// and checks that the text lands in the box, editable, with no API key set.
//
// It downloads the local model (about 700 MB) and a Chromium, and the long clip records in
// real time, so it runs only with TASKTO_DICTATION_E2E=1, never in CI: `make dictation-e2e`.
//
//   - TASKTO_DICTATION_IT_CACHE keeps the model between runs, as for TestRealLocalModels.
//   - TASKTO_DICTATION_E2E_LONG=0 skips the clip of nearly 10 minutes.
//   - The Portuguese clip is made with macOS's `say` and its pt_BR voice Luciana, so it is
//     skipped where that isn't installed.

import assert from 'node:assert/strict'
import { execFileSync, spawn } from 'node:child_process'
import { createHash } from 'node:crypto'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, before, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'

const enabled = process.env.TASKTO_DICTATION_E2E === '1'
const repo = fileURLToPath(new URL('../..', import.meta.url))
const email = 'owner@example.com'
const password = 'dictation-e2e-password'

let dir
let server
let base
let cookie
let ws

before(async () => {
  if (!enabled) return
  dir = mkdtempSync(join(tmpdir(), 'tt-dict-'))
  const args = ['-addr', '127.0.0.1:0', '-data-dir', join(dir, 'data'), '-insecure-cookies', '-agent']
  args.push('-agent-entry', join(repo, 'agent/dist/index.js'))
  if (process.env.TASKTO_DICTATION_IT_CACHE) args.push('-dictation-cache', process.env.TASKTO_DICTATION_IT_CACHE)
  server = spawn(join(repo, 'bin/taskto'), args, { stdio: ['ignore', 'ignore', 'pipe'] })
  let logs = ''
  const listening = new Promise((resolve, reject) => {
    const giveUp = setTimeout(() => reject(new Error(`the server didn't start in a minute:\n${logs}`)), 60_000)
    server.stderr.on('data', (b) => {
      logs += b
      // The setup token comes first; wait for both all the same.
      const m = logs.match(/taskto listening addr=(\S+).*web_app_built=(\w+)/)
      const token = logs.match(/setup_token=([A-Za-z0-9_-]{43})/)
      if (m && token) {
        clearTimeout(giveUp)
        resolve([m[1], m[2], token[1]])
      }
    })
    const fail = (e) => {
      clearTimeout(giveUp)
      reject(e)
    }
    // No bin/taskto: run make build first (make dictation-e2e does).
    server.on('error', (e) => fail(new Error(`the server didn't start: ${e.message}`)))
    server.on('exit', (code) => fail(new Error(`the server exited (${code}):\n${logs}`)))
  })
  const [addr, built, token] = await listening
  assert.equal(built, 'true', 'the web app isn’t built into the binary: run make build (make dictation-e2e does)')
  base = `http://${addr}`

  // The owner, through the API; the browser signs in with the same account.
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

  // No API key: the engine is the local model. Download it before the clips, so the first
  // isn't answered "still downloading" (the web app offers "Try again" for that).
  let st = await call('GET', '/dictation')
  assert.equal(st.engine, 'local')
  assert.equal(st.settings.api.api_key, 'not set')
  assert.equal(st.local?.model, 'parakeet-v3')
  assert.notEqual(st.local.state, 'unavailable', `no local model for this platform: ${st.local.error}`)
  await call('POST', '/dictation/warm', {})
  const deadline = Date.now() + 30 * 60_000
  while (st.local.state !== 'ready') {
    assert.ok(st.local.state !== 'failed' && Date.now() < deadline, `download: ${JSON.stringify(st.local)}`)
    await new Promise((r) => setTimeout(r, 2000))
    st = await call('GET', '/dictation')
  }
})

// The server goes before its data directory: SIGTERM, ten seconds to shut down, then SIGKILL.
after(async () => {
  if (server && server.exitCode === null && server.signalCode === null) {
    const exited = new Promise((resolve) => server.once('exit', resolve))
    server.kill()
    const giveUp = setTimeout(() => server.kill('SIGKILL'), 10_000)
    await exited
    clearTimeout(giveUp)
  }
  if (dir) rmSync(dir, { recursive: true, force: true })
})

// call is the owner's request to the workspace's API.
async function call(method, path, body) {
  const res = await fetch(`${base}/api/workspaces/${ws}${path}`, {
    method,
    headers: { Cookie: cookie, ...(body ? { 'Content-Type': 'application/json' } : {}) },
    body: body && JSON.stringify(body),
    signal: AbortSignal.timeout(60_000),
  })
  assert.ok(res.ok, `${method} ${path}: ${res.status} ${await res.clone().text()}`)
  return res.json()
}

// The clips. sherpa-onnx's English test clip of Parakeet v3 (Kennedy's inaugural address, in
// the public domain), at the pinned commit, as TestRealLocalModels: a canonical 24 kHz WAV.
let english
async function kennedy() {
  if (english) return english
  const url =
    'https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8/resolve/ed5d745623472f97cfc2e6c2916ee248798fc5a8/test_wavs/en.wav'
  const res = await fetch(url, { signal: AbortSignal.timeout(60_000) })
  assert.ok(res.ok, `${url}: ${res.status}`)
  const b = Buffer.from(await res.arrayBuffer())
  assert.equal(
    createHash('sha256').update(b).digest('hex'),
    '148b936b43ce7c546a866e64da059f0458aee2d65e617f16e9d94f06e8d99ed6',
  )
  english = {
    rate: b.readUInt32LE(24),
    samples: new Int16Array(b.buffer.slice(b.byteOffset + 44, b.byteOffset + b.length)),
  }
  return english
}

// writeClip writes clip, repeated times with a second of silence after each, after a second
// of silence, as a WAV file; it returns the path and how long it plays.
function writeClip(name, clip, times = 1) {
  const gap = clip.rate
  const n = gap + times * (clip.samples.length + gap)
  const b = Buffer.alloc(44 + 2 * n)
  b.write('RIFF', 0)
  b.writeUInt32LE(36 + 2 * n, 4)
  b.write('WAVEfmt ', 8)
  b.writeUInt32LE(16, 16)
  b.writeUInt16LE(1, 20) // PCM
  b.writeUInt16LE(1, 22) // mono
  b.writeUInt32LE(clip.rate, 24)
  b.writeUInt32LE(2 * clip.rate, 28)
  b.writeUInt16LE(2, 32)
  b.writeUInt16LE(16, 34)
  b.write('data', 36)
  b.writeUInt32LE(2 * n, 40)
  for (let t = 0, at = 44 + 2 * gap; t < times; t++, at += 2 * (clip.samples.length + gap)) {
    Buffer.from(clip.samples.buffer, clip.samples.byteOffset, 2 * clip.samples.length).copy(b, at)
  }
  const path = join(dir, name)
  writeFileSync(path, b)
  return { path, seconds: n / clip.rate }
}

// wavSeconds is how long a WAV file plays, from its fmt chunk's byte rate and its data
// chunk's size (say writes other chunks before them).
function wavSeconds(b) {
  let byteRate = 0
  let size = 0
  for (let at = 12; at + 8 <= b.length; ) {
    const id = b.toString('latin1', at, at + 4)
    const n = b.readUInt32LE(at + 4)
    if (id === 'fmt ') byteRate = b.readUInt32LE(at + 16)
    if (id === 'data') size = n
    at += 8 + n + (n % 2)
  }
  assert.ok(byteRate > 0 && size > 0, 'not a WAV file')
  return size / byteRate
}

// dictate does what the person does in a Chromium whose microphone plays the clip once, and
// returns the text that lands in the chat box.
async function dictate(clip) {
  const browser = await chromium.launch({
    channel: 'chromium', // the full Chromium, in its new headless mode
    args: [
      '--use-fake-ui-for-media-stream',
      '--use-fake-device-for-media-stream',
      `--use-file-for-fake-audio-capture=${clip.path}%noloop`,
    ],
  })
  try {
    const page = await browser.newPage()
    await page.goto(`${base}/`)
    await page.locator('input[type=email]').fill(email)
    await page.locator('input[type=password]').fill(password)
    await page.getByRole('button', { name: 'Log in' }).click()
    await page.waitForURL(/\/w\/\d+\//)

    await page.goto(`${base}/w/${ws}/conversations`)
    await page.getByRole('button', { name: 'New', exact: true }).click()
    // The chat box by its label; its alerts are the conversation's (a new one has no others).
    const box = page.getByRole('textbox', { name: 'Message', exact: true })
    const alerts = page.getByRole('region', { name: 'Conversation' }).getByRole('alert')
    await page.getByRole('button', { name: 'Dictate' }).click()
    const stop = page.getByRole('button', { name: 'Stop dictation' })
    await stop.waitFor({ timeout: 30_000 })

    // The file plays from when the microphone opens: record all of it, by the page's clock.
    const timer = page.getByRole('timer')
    const deadline = Date.now() + (clip.seconds + 60) * 1000
    for (;;) {
      const clock = (await timer.textContent()) ?? ''
      const m = clock.match(/^(\d+):(\d+)/)
      assert.ok(m, 'the recording clock is gone')
      if (Number(m[1]) * 60 + Number(m[2]) >= clip.seconds + 1) break
      assert.ok(Date.now() < deadline, `the recording clock is at ${clock} after ${clip.seconds + 60} s`)
      await page.waitForTimeout(500)
    }
    await stop.click()
    const started = Date.now()
    // The text in the box, or an alert in the conversation: the dictation's error, or the composer's.
    let got
    for (const until = Date.now() + 6 * 60_000; !got; await page.waitForTimeout(250)) {
      assert.ok(Date.now() < until, 'no text and no error after 6 minutes')
      const text = await box.inputValue()
      if (text.trim()) got = { text }
      else {
        const alert = (await alerts.allTextContents()).join(' | ')
        if (alert) got = { alert }
      }
    }
    assert.equal(got.alert, undefined, `dictation failed: ${got.alert}`)
    console.log(`${clip.seconds.toFixed(1)} s of audio, text after ${Date.now() - started} ms: ${got.text}`)

    // The text is the person's to edit before sending.
    assert.ok(await box.isEditable())
    await box.press('End')
    await box.pressSequentially(' (edited)')
    assert.equal(await box.inputValue(), `${got.text} (edited)`)
    return got.text
  } finally {
    await browser.close()
  }
}

test('English', { skip: !enabled && 'set TASKTO_DICTATION_E2E=1' }, async () => {
  const text = await dictate(writeClip('en.wav', await kennedy()))
  assert.match(text.toLowerCase(), /ask not what your country can do for you/)
})

test('Portuguese', { skip: !enabled && 'set TASKTO_DICTATION_E2E=1' }, async (t) => {
  const said = 'Bom dia. Por favor, mude a reunião de planejamento para quinta-feira à tarde.'
  const path = join(dir, 'pt.wav')
  let voices = ''
  try {
    voices = execFileSync('say', ['-v', '?'], { encoding: 'utf8' })
  } catch {}
  if (!/^Luciana\b.*pt_BR/m.test(voices)) {
    t.skip('no `say` with the pt_BR voice Luciana (macOS) to make the clip')
    return
  }
  execFileSync('say', ['-v', 'Luciana', '--data-format=LEI16@48000', '-o', path, `[[slnc 1000]] ${said} [[slnc 1000]]`])
  const text = await dictate({ path, seconds: wavSeconds(readFileSync(path)) })
  assert.match(text.toLowerCase(), /reunião de planejamento para quinta-feira à tarde/)
})

test('nearly ten minutes', {
  skip:
    (!enabled && 'set TASKTO_DICTATION_E2E=1') ||
    (process.env.TASKTO_DICTATION_E2E_LONG === '0' && 'TASKTO_DICTATION_E2E_LONG=0'),
}, async () => {
  // The English clip with a second of silence after each, as many times as fit in 9 min 30 s.
  const clip = await kennedy()
  const times = Math.floor((570 - 1) / (clip.samples.length / clip.rate + 1))
  const text = await dictate(writeClip('long.wav', clip, times))
  const heard = text.toLowerCase().split('your country').length - 1
  console.log(`"your country" ${heard} times of ${2 * times}`)
  assert.ok(heard >= Math.floor(2 * times * 0.9), `"your country" ${heard} times of ${2 * times}`)
})

test('glossary', { skip: !enabled && 'set TASKTO_DICTATION_E2E=1' }, async (t) => {
  // A term the model writes as two words: the space's glossary.md joins them. It goes
  // afterwards, so the other clips never depend on running before this one.
  const { spaces } = await call('GET', '/spaces')
  const doc = `/spaces/${spaces[0].id}/docs/glossary.md`
  await call('PUT', doc, { content: '# Glossary\n\n- **YourCountry**: one word here\n' })
  t.after(() => call('DELETE', doc))
  const text = await dictate(writeClip('glossary.wav', await kennedy()))
  assert.match(text, /YourCountry/)
  assert.doesNotMatch(text.toLowerCase(), /your country/)
})
