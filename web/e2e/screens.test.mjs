// The parts of `make screens` (screens.mjs) that decide whether it can be trusted, tested without
// a browser and without the real server: the screen list, starting and stopping the server (against
// tiny fake server processes, a Node child that listens, one that says the web app isn't built, one
// that never says anything), the cleanup on an ordinary end and on a real signal, the script's own
// HTTP client, what it leaves in the output folder, and the ready check of every capture. The
// script only runs when it is the program being started, so importing it starts nothing. Nothing
// here waits for a fixed time: each test waits for the event it is about.

import { spawn } from 'node:child_process'
import { EventEmitter } from 'node:events'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { afterEach, describe, expect, it } from 'vitest'
import {
  ApiFailure,
  awaitReady,
  captureOne,
  captures,
  createCleanup,
  createClient,
  hide,
  launch,
  MANIFEST,
  PHASES,
  readManifest,
  recordCaptures,
  redact,
  SCREENS,
  SETTINGS_HEADINGS,
  SETTINGS_TABS,
  SIZES,
  THEMES,
  trapSignals,
  withoutPlaywrightDebug,
} from './screens.mjs'

const scriptPath = fileURLToPath(new URL('./screens.mjs', import.meta.url))
const scriptUrl = pathToFileURL(scriptPath).href

// A made-up token of the length the server's is.
const FAKE_TOKEN = 'x'.repeat(43)
const LISTENING = `taskto listening addr=127.0.0.1:1 web_app_built=true setup_token=${FAKE_TOKEN}`
const HOLD = 'setInterval(() => {}, 1e6)'

// Tiny stand-ins for the server: Node children that run a script.
const fakes = {
  listening: `console.error(${JSON.stringify(LISTENING)}); ${HOLD}`,
  notBuilt: `console.error(${JSON.stringify(LISTENING.replace('=true', '=false'))}); ${HOLD}`,
  silent: HOLD,
  refuses: `console.error('ERROR taskto stopped err="must not be able to act as root"'); process.exit(3)`,
  stubborn: `process.on('SIGTERM', () => {}); console.error(${JSON.stringify(LISTENING)}); ${HOLD}`,
}
const start = (script, options = {}) =>
  launch({ bin: process.execPath, args: ['-e', script], env: process.env, ...options })

const alive = (pid) => {
  try {
    process.kill(pid, 0)
    return true
  } catch {
    return false
  }
}

const temps = []
const temp = () => {
  const dir = mkdtempSync(join(tmpdir(), 'tt-screens-test-'))
  temps.push(dir)
  return dir
}
afterEach(() => {
  for (const dir of temps.splice(0)) rmSync(dir, { recursive: true, force: true })
})

describe('screens', () => {
  it('names every capture once, as a file that compares between runs', () => {
    const ids = captures().map((c) => c.id)
    expect(new Set(ids).size).toBe(ids.length)
    for (const id of ids) expect(id).toMatch(/^[a-z]+-(light|dark)\/[a-z0-9-]+$/)
  })

  it('has no screen name twice and only known phases', () => {
    const names = SCREENS.map((s) => s.name)
    expect(new Set(names).size).toBe(names.length)
    for (const s of SCREENS) expect(PHASES).toContain(s.phase)
  })

  it('captures at the three sizes in both themes', () => {
    expect(SIZES.map((s) => [s.width, s.height])).toEqual([
      [1440, 900],
      [820, 1100],
      [390, 844],
    ])
    expect(THEMES).toEqual(['light', 'dark'])
    const dirs = new Set(captures().map((c) => c.dir))
    expect(dirs.size).toBe(SIZES.length * THEMES.length)
  })

  it('goes through every tab of the settings page', () => {
    const source = readFileSync(new URL('../src/settings/SettingsPage.tsx', import.meta.url), 'utf8')
    const tabs = [...source.matchAll(/\{ tab: '([a-z-]+)', label:/g)].map((m) => m[1])
    expect(tabs.length).toBeGreaterThan(0)
    expect(SETTINGS_TABS).toEqual(tabs)
  })

  it('lists, for each screen, exactly the sizes and themes it is captured at', () => {
    const all = captures()
    for (const screen of SCREENS) {
      const sizes = screen.only ? SIZES.filter((size) => size.name === screen.only) : SIZES
      const expected = sizes.flatMap((size) => THEMES.map((theme) => `${size.name}-${theme}/${screen.name}`))
      expect(all.filter((c) => c.screen === screen).map((c) => c.id)).toEqual(expected)
    }
    // Only the drawer is for one size, and the phone's.
    expect(SCREENS.filter((s) => s.only).map((s) => [s.name, s.only])).toEqual([['drawer', 'phone']])
    // Each Settings tab is captured at every size and theme, under its own name.
    for (const tab of SETTINGS_TABS) {
      const ids = all.filter((c) => c.screen.name === `settings-${tab}`).map((c) => c.id)
      expect(ids).toHaveLength(SIZES.length * THEMES.length)
    }
    expect(all).toHaveLength(SCREENS.reduce((n, s) => n + (s.only ? 1 : SIZES.length) * THEMES.length, 0))
  })

  it('has no fixed sleep: a ready check decides when a screen is there', () => {
    expect(readFileSync(scriptPath, 'utf8')).not.toMatch(/waitForTimeout/)
  })
})

describe('ready checks', () => {
  // A page whose every locator is the same stand-in: enough to see what a ready check asks for.
  const everything = new Proxy(() => {}, { get: () => () => everything })
  const stand = () => ({
    page: new Proxy({}, { get: () => () => everything }),
    ids: new Proxy({}, { get: () => 'x' }),
    agent: true,
  })

  it('gives every screen a ready check of its own that asks for something', () => {
    for (const screen of SCREENS) {
      expect(typeof screen.ready, screen.name).toBe('function')
      expect([screen.ready(stand())].flat().length, screen.name).toBeGreaterThan(0)
    }
    for (const tab of SETTINGS_TABS) expect(SETTINGS_HEADINGS[tab]).toBeTruthy()
  })

  const locator = (shows, calls = []) => ({
    first: () => ({
      waitFor: async (options) => {
        calls.push(options)
        if (!shows) throw new Error('locator.waitFor: Timeout 1ms exceeded.\nCall log:\n  - waiting for it')
      },
    }),
  })

  it('waits for every locator of the check, visible', async () => {
    const calls = []
    await awaitReady({ name: 'x', ready: () => [locator(true, calls), locator(true, calls)] }, {}, 123)
    expect(calls).toEqual([
      { state: 'visible', timeout: 123 },
      { state: 'visible', timeout: 123 },
    ])
  })

  it('fails a screen whose check never appears, or that has none', async () => {
    await expect(awaitReady({ name: 'setup', ready: () => [locator(true), locator(false)] }, {})).rejects.toThrow(
      /setup is not what the screen shows/,
    )
    await expect(awaitReady({ name: 'setup' }, {})).rejects.toThrow(/no ready check/)
    await expect(awaitReady({ name: 'setup', ready: () => [] }, {})).rejects.toThrow(/empty ready check/)
  })

  const page = (saved) => ({
    waitForLoadState: async () => {},
    evaluate: async () => {},
    screenshot: async ({ path }) => {
      writeFileSync(path, 'not a real picture')
      saved.push(path)
    },
  })
  const capture = (screen) => ({ id: 'desktop-light/setup', dir: 'desktop-light', screen })

  it('saves a capture once its check shows, and says so', async () => {
    const outDir = temp()
    const saved = []
    const written = new Set()
    const c = capture({ name: 'setup', show: async () => {}, ready: () => locator(true) })
    expect(await captureOne(c, { page: page(saved) }, { outDir, written })).toEqual({ ok: true })
    expect(saved).toEqual([join(outDir, 'desktop-light/setup.png')])
    expect([...written]).toEqual(['desktop-light/setup'])
  })

  it('does not save a screen whose check is missing: a failed capture, no picture', async () => {
    const outDir = temp()
    const saved = []
    const written = new Set()
    // A stand-in server that answers every path with a generic page: the screen's heading isn't on it.
    const wrong = capture({ name: 'setup', show: async () => {}, ready: () => locator(false) })
    const result = await captureOne(wrong, { page: page(saved) }, { outDir, written })
    expect(result.ok).toBe(false)
    expect(result.error).toMatch(/^setup is not what the screen shows/)
    expect(saved).toEqual([])
    expect(written.size).toBe(0)
    expect(existsSync(join(outDir, 'desktop-light/setup.png'))).toBe(false)

    const none = await captureOne(
      capture({ name: 'setup', show: async () => {} }),
      { page: page(saved) },
      { outDir, written },
    )
    expect(none.ok).toBe(false)
    expect(saved).toEqual([])
  })

  it('fails a capture whose screen could not be brought up', async () => {
    const saved = []
    const c = capture({
      name: 'setup',
      show: async () => {
        throw new Error('no such button')
      },
      ready: () => locator(true),
    })
    const result = await captureOne(c, { page: page(saved) }, { outDir: temp(), written: new Set() })
    expect(result).toEqual({ ok: false, error: 'no such button' })
    expect(saved).toEqual([])
  })
})

describe('launch', () => {
  it('resolves when the server says it listens, hides its setup token, and stops it', async () => {
    const server = await start(fakes.listening)
    expect(server.base).toBe('http://127.0.0.1:1')
    expect(server.token).toBe(FAKE_TOKEN)
    expect(server.logs()).not.toContain(FAKE_TOKEN)
    expect(server.logs()).toContain('setup_token=[hidden]')
    const pid = server.proc.pid
    expect(alive(pid)).toBe(true)
    await server.stop()
    expect(alive(pid)).toBe(false)
  })

  it('stops and reaps its child when the web app is not built', async () => {
    const err = await start(fakes.notBuilt).catch((e) => e)
    expect(err.message).toMatch(/web app is not built/)
    expect(alive(err.server.proc.pid)).toBe(false)
  })

  it('stops and reaps its child when the server never says it listens', async () => {
    const err = await start(fakes.silent, { timeoutMs: 100 }).catch((e) => e)
    expect(err.message).toMatch(/did not start in time/)
    expect(alive(err.server.proc.pid)).toBe(false)
  })

  it('keeps what a server that exits says, for the reason it gives', async () => {
    const err = await start(fakes.refuses).catch((e) => e)
    expect(err.message).toMatch(/the server exited \(3\)/)
    expect(err.server.logs()).toMatch(/must not be able to act as root/)
  })

  it('rejects, with nothing left, when the program does not exist', async () => {
    const err = await launch({ bin: join(temp(), 'missing'), args: [], env: {} }).catch((e) => e)
    expect(err.message).toMatch(/did not start/)
    expect(err.message).not.toContain(tmpdir())
  })

  it('kills a server that ignores SIGTERM', async () => {
    const server = await start(fakes.stubborn, { killAfterMs: 50 })
    const pid = server.proc.pid
    await server.stop()
    expect(alive(pid)).toBe(false)
  })

  it('hands the child to `track` as soon as it exists, before it is ready', async () => {
    let tracked
    const pending = start(fakes.silent, { timeoutMs: 100, track: (server) => (tracked = server) })
    expect(tracked.proc.pid).toBeGreaterThan(0)
    await pending.catch(() => {})
    expect(alive(tracked.proc.pid)).toBe(false)
  })
})

describe('the cleanup', () => {
  it('runs what was added once, last first, awaited, and goes on past a failure', async () => {
    const cleanup = createCleanup()
    const order = []
    cleanup.add(async () => {
      await Promise.resolve()
      order.push('directory')
    })
    cleanup.add(() => {
      throw new Error('the server would not stop')
    })
    cleanup.add(() => order.push('browser'))
    await Promise.all([cleanup.run(), cleanup.run()])
    await cleanup.run()
    expect(order).toEqual(['browser', 'directory'])
    expect(cleanup.closing).toBe(true)
  })

  it('undoes at once what is added after it began, such as a server spawned as it was interrupted', async () => {
    const cleanup = createCleanup()
    await cleanup.run()
    let undone = false
    cleanup.add(() => {
      undone = true
    })
    await Promise.resolve()
    await Promise.resolve()
    expect(undone).toBe(true)
  })

  it('has a synchronous last resort for a process that is leaving', () => {
    const cleanup = createCleanup()
    const seen = []
    cleanup.add(
      async () => {},
      () => seen.push('first'),
    )
    cleanup.add(
      async () => {},
      () => {
        throw new Error('already gone')
      },
    )
    cleanup.add(
      async () => {},
      () => seen.push('last'),
    )
    cleanup.last()
    expect(seen).toEqual(['last', 'first'])
  })

  it('answers each signal with the cleanup, then the conventional exit code', async () => {
    const codes = { SIGINT: 130, SIGTERM: 143, SIGHUP: 129 }
    for (const [signal, code] of Object.entries(codes)) {
      const target = new EventEmitter()
      const cleanup = createCleanup()
      let cleaned = false
      cleanup.add(async () => {
        await Promise.resolve()
        cleaned = true
      })
      let leave
      const left = new Promise((resolve) => {
        leave = resolve
      })
      const untrap = trapSignals(cleanup, { target, exit: (n) => leave({ n, cleaned }), say: () => {} })
      expect(target.listenerCount(signal)).toBe(1)
      target.emit(signal)
      // The process leaves only after the cleanup is done.
      expect(await left).toEqual({ n: code, cleaned: true })
      untrap()
      expect(target.listenerCount(signal)).toBe(0)
    }
  })

  // A real Node process that does what the script does at its start (traps the signals, then
  // spawns a fake server and waits), given a real signal. `startup` is the signal arriving while
  // the server is starting, `running` while it waits for something else, such as the setup request.
  const interrupted = async (mode, signal, code) => {
    const dir = temp()
    const child = spawn(
      process.execPath,
      [
        '--input-type=module',
        '-e',
        `
        import { createCleanup, trapSignals, launch } from ${JSON.stringify(scriptUrl)}
        import { rmSync } from 'node:fs'
        const [dir, fake] = process.argv.slice(1)
        const cleanup = createCleanup()
        trapSignals(cleanup, { say: () => {} })
        cleanup.add(() => rmSync(dir, { recursive: true, force: true }))
        launch({
          bin: process.execPath,
          args: ['-e', fake],
          env: process.env,
          track: (server) => {
            cleanup.add(() => server.stop(), () => server.kill())
            console.log('spawned ' + server.proc.pid)
          },
        }).then(
          () => { console.log('ready'); setInterval(() => {}, 1e6) },
          (err) => console.log('failed ' + err.message),
        )
        `,
        dir,
        mode === 'startup' ? fakes.silent : fakes.listening,
      ],
      { stdio: ['ignore', 'pipe', 'inherit'] },
    )
    let out = ''
    let serverPid
    const waits = []
    child.stdout.on('data', (chunk) => {
      out += chunk
      const spawned = out.match(/spawned (\d+)/)
      if (spawned) serverPid = Number(spawned[1])
      for (const wait of waits.splice(0)) wait()
    })
    const until = async (marker) => {
      while (!out.includes(marker)) await new Promise((resolve) => waits.push(resolve))
    }
    const exited = new Promise((resolve) => child.once('exit', (c, s) => resolve({ code: c, signal: s })))
    await until(mode === 'startup' ? 'spawned' : 'ready')
    expect(alive(serverPid)).toBe(true)
    child.kill(signal)
    expect(await exited).toEqual({ code, signal: null })
    expect(alive(serverPid)).toBe(false)
    expect(existsSync(dir)).toBe(false)
  }

  it('on SIGINT while the server is starting: the server is reaped, the directory removed, exit 130', () =>
    interrupted('startup', 'SIGINT', 130))
  it('on SIGINT with the server up and the run waiting: the same', () => interrupted('running', 'SIGINT', 130))
  it('on SIGTERM: exit 143, nothing left', () => interrupted('startup', 'SIGTERM', 143))
  it('on SIGHUP: exit 129, nothing left', () => interrupted('running', 'SIGHUP', 129))
})

describe('the script’s own HTTP client', () => {
  // Made-up values of what must never be printed.
  const COOKIE = 'fake-session-value-for-test'
  const TOKEN = 'fake-setup-token-for-test'
  const PASSWORD = 'fake-password-for-test'

  const serve = async () => {
    const seen = []
    const server = createServer((req, res) => {
      let body = ''
      req.on('data', (chunk) => {
        body += chunk
      })
      req.on('end', () => {
        seen.push({ method: req.method, url: req.url, cookie: req.headers.cookie, type: req.headers['content-type'] })
        res.setHeader('Content-Type', 'application/json')
        if (req.url === '/api/setup') {
          res.setHeader('Set-Cookie', [`tt_session=${COOKIE}; Path=/; HttpOnly`, 'other=1; Path=/'])
          return res.end(JSON.stringify({ ok: true }))
        }
        if (req.url === '/api/logout') {
          res.setHeader('Set-Cookie', 'other=; Path=/; Max-Age=0')
          res.statusCode = 204
          return res.end()
        }
        // A failing server that echoes everything it was sent, as an unlucky one might.
        res.statusCode = req.url === '/api/denied' ? 401 : 500
        res.end(JSON.stringify({ code: 'sample_code', error: `${req.headers.cookie} ${body}` }))
      })
    })
    await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
    return { server, seen, base: `http://127.0.0.1:${server.address().port}` }
  }

  it('keeps the session cookie in memory, sends it back, and hands it to a browser context', async () => {
    const { server, seen, base } = await serve()
    try {
      const client = createClient(base)
      expect(await client.call('POST', '/api/setup', { password: PASSWORD, setup_token: TOKEN })).toEqual({ ok: true })
      expect(client.cookies()).toEqual([
        { name: 'tt_session', value: COOKIE, url: base },
        { name: 'other', value: '1', url: base },
      ])
      await expect(client.call('GET', '/api/other')).rejects.toBeInstanceOf(ApiFailure)
      expect(seen[1].cookie).toBe(`tt_session=${COOKIE}; other=1`)
      expect(seen[0].type).toBe('application/json')
      expect(await client.call('POST', '/api/logout', {})).toBeNull()
      expect(client.cookies().map((c) => c.name)).toEqual(['tt_session'])
    } finally {
      server.close()
    }
  })

  it('reports a failed call as its method, path and status or code, never what was sent or got', async () => {
    const { server, base } = await serve()
    try {
      const client = createClient(base)
      await client.call('POST', '/api/setup', { password: PASSWORD, setup_token: TOKEN })
      const failed = await client
        .call('POST', '/api/seed', { password: PASSWORD, setup_token: TOKEN })
        .catch((err) => err)
      expect(failed).toBeInstanceOf(ApiFailure)
      expect(failed.message).toBe('POST /api/seed: 500 (sample_code)')
      const denied = await client.call('GET', '/api/denied').catch((err) => err)
      expect(denied.message).toBe('GET /api/denied: 401 (sample_code)')
      for (const err of [failed, denied]) {
        for (const secret of [COOKIE, TOKEN, PASSWORD]) {
          expect(err.message).not.toContain(secret)
          expect(String(err.stack)).not.toContain(secret)
          expect(JSON.stringify({ ...err })).not.toContain(secret)
        }
      }
    } finally {
      server.close()
    }
  })

  it('reports a call that got no answer by its error code, with no address and no raw error', async () => {
    const { server, base } = await serve()
    await new Promise((resolve) => server.close(resolve))
    const err = await createClient(base)
      .call('POST', '/api/setup', { password: PASSWORD })
      .catch((e) => e)
    expect(err.message).toBe('POST /api/setup: no answer (ECONNREFUSED)')
    expect(err.message).not.toContain('127.0.0.1')
    expect(err.message).not.toContain(PASSWORD)
  })

  it('redacts the setup token and every value it was told to hide', () => {
    hide('a-value-to-hide')
    expect(redact(`setup_token=${FAKE_TOKEN} and a-value-to-hide`)).toBe('setup_token=[hidden] and [hidden]')
  })

  it('turns off Playwright’s debug traces, which print every call’s arguments, and keeps the rest', () => {
    expect(withoutPlaywrightDebug('pw:channel')).toBeUndefined()
    expect(withoutPlaywrightDebug('pw:api, other:thing pw:browser*')).toBe('other:thing')
    expect(withoutPlaywrightDebug(undefined)).toBeUndefined()
  })
})

describe('the output folder', () => {
  const put = (outDir, id, content = 'picture') => {
    mkdirSync(join(outDir, id.split('/')[0]), { recursive: true })
    writeFileSync(join(outDir, `${id}.png`), content)
  }
  const has = (outDir, id) => existsSync(join(outDir, `${id}.png`))

  it('never deletes a file it did not write, in a full run or after a failed one', () => {
    const outDir = temp()
    mkdirSync(join(outDir, 'desktop-light'), { recursive: true })
    writeFileSync(join(outDir, 'desktop-light/foreign.png'), 'someone else’s picture')
    writeFileSync(join(outDir, 'notes.txt'), 'notes')
    writeFileSync(join(outDir, 'desktop-light/readme.md'), 'readme')

    put(outDir, 'desktop-light/boards')
    recordCaptures({ outDir, written: new Set(['desktop-light/boards']), prune: true })
    // A later full run that failed somewhere, and one that succeeded, with no pictures at all.
    recordCaptures({ outDir, written: new Set(), failed: ['desktop-light/boards'], prune: false })
    recordCaptures({ outDir, written: new Set(), prune: true })

    expect(readFileSync(join(outDir, 'desktop-light/foreign.png'), 'utf8')).toBe('someone else’s picture')
    expect(readFileSync(join(outDir, 'notes.txt'), 'utf8')).toBe('notes')
    expect(existsSync(join(outDir, 'desktop-light/readme.md'))).toBe(true)
  })

  it('prunes what an earlier run wrote and this one did not, only after a run that failed nowhere', () => {
    const outDir = temp()
    for (const id of ['desktop-light/boards', 'desktop-light/old-screen', 'phone-dark/drawer']) put(outDir, id)
    recordCaptures({
      outDir,
      written: new Set(['desktop-light/boards', 'desktop-light/old-screen', 'phone-dark/drawer']),
    })
    expect([...readManifest(outDir)].sort()).toEqual([
      'desktop-light/boards',
      'desktop-light/old-screen',
      'phone-dark/drawer',
    ])

    // A run with a failure leaves the earlier run's others in place.
    recordCaptures({ outDir, written: new Set(['desktop-light/boards']), prune: false })
    expect(has(outDir, 'desktop-light/old-screen')).toBe(true)

    // A full run that failed nowhere replaces them.
    const { removed } = recordCaptures({ outDir, written: new Set(['desktop-light/boards']), prune: true })
    expect(removed).toEqual(['desktop-light/old-screen', 'phone-dark/drawer'])
    expect(has(outDir, 'desktop-light/old-screen')).toBe(false)
    expect(has(outDir, 'phone-dark/drawer')).toBe(false)
    expect(has(outDir, 'desktop-light/boards')).toBe(true)
    expect([...readManifest(outDir)]).toEqual(['desktop-light/boards'])
  })

  it('in a filtered run, drops the old picture of a selected capture that failed or was skipped, and keeps the rest', () => {
    const outDir = temp()
    const ids = ['desktop-light/boards', 'desktop-light/conversation', 'desktop-light/home']
    for (const id of ids) put(outDir, id, 'old')
    recordCaptures({ outDir, written: new Set(ids) })
    // The run selected boards (failed) and conversation (skipped, no agent runtime); home was not selected.
    recordCaptures({
      outDir,
      written: new Set(),
      failed: ['desktop-light/boards', 'desktop-light/conversation'],
      prune: false,
    })
    expect(has(outDir, 'desktop-light/boards')).toBe(false)
    expect(has(outDir, 'desktop-light/conversation')).toBe(false)
    expect(has(outDir, 'desktop-light/home')).toBe(true)
    expect([...readManifest(outDir)]).toEqual(['desktop-light/home'])
  })

  it('removes the old picture of a capture that failed, since it would pass for the screen', () => {
    const outDir = temp()
    put(outDir, 'desktop-light/boards', 'old')
    recordCaptures({ outDir, written: new Set(['desktop-light/boards']) })
    recordCaptures({ outDir, written: new Set(), failed: ['desktop-light/boards'] })
    expect(has(outDir, 'desktop-light/boards')).toBe(false)
  })

  it('does not trust a manifest that names anything but its own kind of file', () => {
    const outDir = temp()
    const victim = join(temp(), 'victim.png')
    writeFileSync(victim, 'not yours')
    writeFileSync(
      join(outDir, MANIFEST),
      JSON.stringify({ captures: [`../${victim}`, victim, '../../etc/passwd', 'desktop-light/ok', 7] }),
    )
    expect([...readManifest(outDir)]).toEqual(['desktop-light/ok'])
    recordCaptures({ outDir, written: new Set(), prune: true })
    expect(existsSync(victim)).toBe(true)
  })

  it('starts from nothing when there is no manifest, or a broken one', () => {
    const outDir = temp()
    expect(readManifest(outDir).size).toBe(0)
    writeFileSync(join(outDir, MANIFEST), '{ not json')
    expect(readManifest(outDir).size).toBe(0)
  })
})
