// Captures every screen of the web app, for reviews of UI changes (`make screens`,
// docs/design.md#capturing-the-screens): the built server on an empty temporary data directory,
// sample data seeded through the HTTP API, and a real Chromium (Playwright) that visits each
// screen at three sizes in a light and a dark theme.
//
//   node web/e2e/screens.mjs [--out DIR] [--only TEXT[,TEXT...]] [--list] [--strict]
//
//   --out DIR      where the PNGs go (default web/screens/, which git ignores); TASKTO_SCREENS_OUT
//   --only TEXT    only the captures whose name (`desktop-light/boards`) contains one of the
//                  comma-separated texts; TASKTO_SCREENS_ONLY
//   --list         print the capture names and stop
//   --strict       also exit non-zero when the browser logged an error or a request failed
//   --unsafe-agent start the server with -unsafe-privileged-agents when it refuses the agent
//                  runtime (see below); TASKTO_SCREENS_UNSAFE_AGENT=1
//
// The files are named `<size>-<theme>/<screen>.png`, the same on every run, so two runs (before
// and after a change) can be compared file by file. Whatever the outcome, the server is stopped
// and the data directory removed. It never runs in CI: it needs a built server (`make build`) and
// takes minutes.
//
// The agent runtime: a conversation with replies needs the server started with -agent, where a
// scripted offline model answers. The server refuses that when its user can run sudo without a
// password (docs/agent-safety.md). Then the script starts the server without the agent, captures
// the conversation screens with no replies, and says so. -unsafe-agent (or
// TASKTO_SCREENS_UNSAFE_AGENT=1) passes -unsafe-privileged-agents, which turns that refusal into a
// warning: for a development machine only, never a server people use.
//
// Everything seeded is invented: example.com addresses and made-up names.

import { spawn } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readdirSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { parseArgs } from 'node:util'

const repo = fileURLToPath(new URL('../..', import.meta.url))

export const SIZES = [
  { name: 'desktop', width: 1440, height: 900 },
  { name: 'tablet', width: 820, height: 1100 },
  { name: 'phone', width: 390, height: 844 },
]
export const THEMES = ['light', 'dark']

export const SETTINGS_TABS = [
  'general',
  'members',
  'spaces',
  'providers',
  'cli-accounts',
  'search',
  'dictation',
  'notifications',
  'password',
  'tokens',
  'about',
]

const owner = {
  email: 'ana@example.com',
  name: 'Ana Ribeiro',
  password: 'sample-password-for-screens',
  workspace: 'Acme Studio',
}

// What a screen needs, in the order the run goes: before any account exists, with an account and
// nothing in it, with the sample data, and signed out again.
export const PHASES = ['first-run', 'empty', 'seeded', 'signed-out']

// Each screen: a stable name (the file's), its phase, and `show(s)`, which brings the screen up.
// `s` has `page`, `go(path)`, `ids` (the seeded data) and `path(section, item)`. A screen that
// can't find what it clicks throws, so a renamed control fails the run and doesn't silently
// capture the wrong thing. `only: 'phone'` limits a screen to one size; `needsAgent` marks a
// screen that exists only where the agent runtime runs (a conversation can't be made without it).
export const SCREENS = [
  { name: 'setup', phase: 'first-run', show: (s) => s.go('/') },
  { name: 'login', phase: 'signed-out', show: (s) => s.go('/') },
  ...['home', 'conversations', 'knowledge', 'boards', 'apps', 'inbox'].map((section) => ({
    name: `empty-${section}`,
    phase: 'empty',
    show: (s) => s.go(s.path(section)),
  })),
  { name: 'home', phase: 'seeded', show: (s) => s.go(s.path('home')) },
  {
    name: 'home-customise',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('home'))
      await s.page.getByRole('button', { name: 'Customize' }).first().click()
    },
  },
  { name: 'conversations', phase: 'seeded', show: (s) => s.go(s.path('conversations')) },
  {
    name: 'conversation',
    phase: 'seeded',
    needsAgent: true,
    show: (s) => s.go(s.path('conversations', s.ids.conversation)),
  },
  { name: 'knowledge', phase: 'seeded', show: (s) => s.go(s.path('knowledge')) },
  {
    name: 'knowledge-document',
    phase: 'seeded',
    show: (s) => s.go(s.path('knowledge', `${s.ids.space}:welcome.md`)),
  },
  {
    name: 'knowledge-editor',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('knowledge', `${s.ids.space}:welcome.md`))
      await s.page.getByRole('button', { name: 'Edit', exact: true }).first().click()
    },
  },
  {
    name: 'knowledge-search',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('knowledge'))
      const box = s.page.getByRole('searchbox').first()
      await box.fill('launch')
      await box.press('Enter')
      await s.page.getByText('Launch plan').first().waitFor()
    },
  },
  { name: 'boards', phase: 'seeded', show: (s) => s.go(s.path('boards')) },
  { name: 'board', phase: 'seeded', show: (s) => s.go(s.path('boards', `${s.ids.space}:${s.ids.board}`)) },
  {
    name: 'board-shared',
    phase: 'seeded',
    show: (s) => s.go(s.path('boards', `${s.ids.team}:${s.ids.teamBoard}`)),
  },
  {
    name: 'card-open',
    phase: 'seeded',
    show: (s) => s.go(s.path('boards', `${s.ids.space}:${s.ids.board}:${s.ids.card}`)),
  },
  {
    name: 'card-add',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('boards', `${s.ids.space}:${s.ids.board}`))
      await s.page.getByRole('button', { name: 'Add a card' }).first().click()
    },
  },
  { name: 'apps', phase: 'seeded', show: (s) => s.go(s.path('apps')) },
  { name: 'apps-templates', phase: 'seeded', show: (s) => s.go(s.path('apps', 'templates')) },
  { name: 'app-open', phase: 'seeded', show: (s) => s.go(s.path('apps', `${s.ids.space}-${s.ids.app}`)) },
  { name: 'app-builder', phase: 'seeded', show: (s) => s.go(s.path('apps', `edit-${s.ids.space}-${s.ids.app}`)) },
  { name: 'inbox', phase: 'seeded', show: (s) => s.go(s.path('inbox')) },
  ...SETTINGS_TABS.map((tab) => ({
    name: `settings-${tab}`,
    phase: 'seeded',
    show: (s) => s.go(s.path('settings', tab)),
  })),
  {
    name: 'account-menu',
    phase: 'seeded',
    show: async (s) => {
      await s.go(s.path('home'))
      await s.page.locator('.tt-account summary').click()
    },
  },
  {
    name: 'drawer',
    phase: 'seeded',
    only: 'phone',
    show: async (s) => {
      await s.go(s.path('home'))
      await s.page.locator('.tt-menu-button').click()
    },
  },
]

// Every capture, in the order the run takes them, as `{ id: 'desktop-light/boards', ... }`.
export function captures() {
  const combos = SIZES.flatMap((size) => THEMES.map((theme) => ({ size, theme, dir: `${size.name}-${theme}` })))
  return PHASES.flatMap((phase) =>
    SCREENS.filter((screen) => screen.phase === phase).flatMap((screen) =>
      combos
        .filter((c) => !screen.only || screen.only === c.size.name)
        .map((c) => ({ id: `${c.dir}/${screen.name}`, screen, ...c })),
    ),
  )
}

// A secret never reaches the output: the setup token is in the server's log while no account
// exists, and a log may be printed when something fails.
const redact = (text) => text.replace(/setup_token=\S+/g, 'setup_token=[hidden]')

// Starts bin/taskto on a fresh data directory and waits until it listens. Resolves to the server,
// or, when it exits first, rejects with an error that carries the log.
function launch(bin, dir, flags) {
  // The server gets its own flags only: a TASKTO_* variable of this machine (a config file, a
  // data directory, a provider) must not change what is captured.
  const env = Object.fromEntries(Object.entries(process.env).filter(([name]) => !name.startsWith('TASKTO_')))
  const proc = spawn(bin, ['-addr', '127.0.0.1:0', '-data-dir', join(dir, 'data'), '-insecure-cookies', ...flags], {
    env,
    stdio: ['ignore', 'ignore', 'pipe'],
  })
  let logs = ''
  const server = {
    proc,
    logs: () => redact(logs),
    stop: async () => {
      proc.removeAllListeners('exit')
      if (proc.exitCode === null && proc.signalCode === null) {
        await new Promise((done) => {
          const kill = setTimeout(() => proc.kill('SIGKILL'), 10_000)
          proc.once('exit', () => {
            clearTimeout(kill)
            done()
          })
          proc.kill('SIGTERM')
        })
      }
    },
  }
  return new Promise((ok, fail) => {
    const giveUp = setTimeout(
      () => fail(Object.assign(new Error('the server did not start in a minute'), { server })),
      60_000,
    )
    const failWith = (err) => {
      clearTimeout(giveUp)
      fail(Object.assign(err, { server }))
    }
    proc.stderr.on('data', (chunk) => {
      logs = (logs + chunk).slice(-100_000)
      const addr = logs.match(/taskto listening addr=(\S+).*web_app_built=(\w+)/)
      const token = logs.match(/setup_token=([A-Za-z0-9_-]{43})/)
      if (addr && token && !server.base) {
        clearTimeout(giveUp)
        if (addr[2] !== 'true') return failWith(new Error('the web app is not built into the binary: run make build'))
        server.base = `http://${addr[1]}`
        server.token = token[1]
        ok(server)
      }
    })
    proc.on('error', (e) => failWith(new Error(`the server did not start: ${e.message}`)))
    proc.on('exit', (code) => failWith(new Error(`the server exited (${code})`)))
  })
}

// The server with the agent runtime when it accepts it. `agent` says what came of that: 'on', or
// why not.
async function startServer(dir, { unsafeAgent }) {
  const bin = process.env.TASKTO_SCREENS_BIN || join(repo, 'bin/taskto')
  const entry = join(repo, 'agent/dist/index.js')
  if (!existsSync(entry)) {
    const server = await launch(bin, dir, [])
    return Object.assign(server, { agent: 'off: the agent runtime is not built (make build builds it)' })
  }
  const agentFlags = ['-agent', '-agent-entry', entry, ...(unsafeAgent ? ['-unsafe-privileged-agents'] : [])]
  try {
    const server = await launch(bin, dir, agentFlags)
    return Object.assign(server, { agent: unsafeAgent ? 'on (-unsafe-privileged-agents)' : 'on' })
  } catch (err) {
    const refused = /must not be able to act as root|act as root/.test(err.server?.logs() ?? '')
    await err.server?.stop()
    if (!refused) throw Object.assign(err, { message: `${err.message}:\n${err.server?.logs() ?? ''}` })
    rmSync(join(dir, 'data'), { recursive: true, force: true })
    const server = await launch(bin, dir, [])
    return Object.assign(server, {
      agent:
        'off: the server refuses the agent runtime for a user who can run sudo without a password (docs/agent-safety.md)',
    })
  }
}

const isoDay = (offset) => {
  const d = new Date()
  d.setDate(d.getDate() + offset)
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}

// The sample data, through the API as the owner. Dates are relative to today, so the labels that
// depend on them (overdue, soon) read the same on every day. Returns the ids the screens use.
async function seed({ api, workspace, agent, say }) {
  const W = `/api/workspaces/${workspace}`
  const { spaces } = await api('GET', `${W}/spaces`)
  const mine = spaces.find((s) => s.kind === 'private') ?? spaces[0]
  const team = await api('POST', `${W}/spaces`, { name: 'Product team' })
  const P = `${W}/spaces/${mine.id}`
  const T = `${W}/spaces/${team.id}`

  const docs = {
    'welcome.md':
      '# Welcome\n\nThis is the **knowledge base**. Notes live here as Markdown.\n\n## Getting started\n\n- Write a note\n- Ask the agent to summarise it\n- Link it to a card\n\n> Everything is versioned in git.\n\n```sh\ntaskto backup --out backup.tar.gz\n```\n\n| Plan | Seats | Price |\n|---|---:|---:|\n| Solo | 1 | 0 |\n| Team | 10 | 49 |\n',
    'launch/plan.md':
      '# Launch plan\n\nShip the beta in two weeks.\n\n1. Finish onboarding\n2. Write the launch post\n3. Record the demo\n',
    'launch/post.md': '# Launch post\n\nDraft of the announcement.\n',
    'meetings/weekly-planning.md':
      '# Weekly planning\n\nAttendees: Ana, Bruno, Carla.\n\n## Decisions\n\n- Keep the launch date.\n',
    'meetings/weekly-planning-with-a-very-long-title-that-should-wrap-or-truncate-gracefully.md':
      '# Weekly\n\nNotes.\n',
    'ideas.md': '# Ideas\n\n- A quotas view\n- Email digests\n',
  }
  for (const [path, content] of Object.entries(docs))
    await api('PUT', `${P}/docs/${path}`, { content, message: `Add ${path}` })
  await api('PUT', `${T}/docs/handbook.md`, { content: '# Handbook\n\nHow the team works.\n', message: 'Add handbook' })

  const cards = [
    [
      'Write the launch post',
      'todo',
      {
        due: isoDay(-3),
        labels: ['launch', 'writing'],
        body: 'Draft, review, publish.\n\n- [ ] Outline\n- [ ] Draft\n',
      },
    ],
    ['Record the demo video', 'todo', { due: isoDay(9), labels: ['launch'] }],
    [
      'Fix the onboarding checklist copy so that it reads well on small phones and long languages',
      'doing',
      { labels: ['onboarding', 'copy', 'i18n'], due: isoDay(1) },
    ],
    ['Review the pricing page', 'doing', {}],
    ['Quotas: design the usage view', 'backlog', { labels: ['design'] }],
    ['Email digests', 'backlog', {}],
    ['Audit log filters', 'backlog', { blocked_reason: 'waiting for the audit log API' }],
    ['Dark mode contrast pass', 'review', { labels: ['design', 'a11y'] }],
    ['Set up backups', 'done', {}],
    ['Invite the team', 'done', {}],
  ]
  const board = (await api('GET', `${P}/boards`)).boards[0]
  for (const [title, column, extra] of cards)
    await api('POST', `${P}/boards/${board.id}/cards`, { title, column, ...extra })
  const card = (await api('GET', `${P}/boards/${board.id}/cards`)).cards.find((c) => c.title === cards[0][0])
  await api('POST', `${P}/boards/${board.id}/cards/${card.id}/comments`, {
    body: 'First outline is in `launch/post.md`.',
  })
  await api('POST', `${P}/boards/${board.id}/cards/${card.id}/comments`, {
    body: 'Looks good. Can we add the pricing table?',
  })

  const teamBoard = await api('POST', `${T}/boards`, { name: 'Launch' })
  await api('POST', `${T}/boards`, { name: 'Support' })
  for (const [title, column, extra] of cards.slice(0, 5))
    await api('POST', `${T}/boards/${teamBoard.id}/cards`, { title, column, ...extra })

  await api('POST', `${W}/invites`, { email: 'bruno@example.com', role: 'member' })
  await api('POST', `${W}/tokens`, { name: 'laptop' })

  // Apps from the templates that need no board, installed and published; their widgets and pins
  // on Home.
  const { templates } = await api('GET', `${W}/app-templates`)
  const apps = []
  for (const template of templates.filter((t) => !t.needs_board).slice(0, 3)) {
    const app = await api('POST', `${P}/app-templates/${template.id}/install`, {})
    await api('POST', `${P}/apps/${app.id}/publish`, { version: app.version, acknowledge: [] })
    apps.push(app)
  }
  const available = (await api('GET', `${W}/home/available`)).apps ?? []
  for (const app of available.slice(0, 3)) {
    for (const widget of (app.widgets ?? []).slice(0, 2)) {
      await api('POST', `${W}/home/items`, {
        kind: 'widget',
        space_id: app.space_id,
        app_id: app.app_id,
        widget: widget.id ?? widget,
      })
    }
    if (app.pin) await api('POST', `${W}/home/items`, { kind: 'pin', space_id: app.space_id, app_id: app.app_id })
  }

  // Conversations: one with an exchange when the scripted model is there to answer. Without the
  // agent runtime the server can't make one.
  let talk
  if (agent.startsWith('on')) {
    talk = await api('POST', `${W}/conversations`, { title: 'Plan the launch week' })
    await api('POST', `${W}/conversations`, { title: 'Summarise the weekly notes' })
    await api('POST', `${W}/conversations`, {})
    let replies = 0
    for (const content of [
      'List the documents in my knowledge base and summarise the launch plan.',
      'Add a card to my board: "Book the launch call".',
    ]) {
      await api('POST', `${W}/conversations/${talk.id}/messages`, { content })
      const deadline = Date.now() + 60_000
      for (;;) {
        const { messages } = await api('GET', `${W}/conversations/${talk.id}/messages`)
        if (messages.filter((m) => m.role === 'assistant').length > replies) break
        if (Date.now() > deadline) throw new Error('the scripted model did not answer in a minute')
        await new Promise((r) => setTimeout(r, 500))
      }
      replies++
    }
  } else {
    say('the agent runtime is off: there are no conversations to capture')
  }

  return {
    space: mine.id,
    team: team.id,
    board: board.id,
    teamBoard: teamBoard.id,
    card: card.id,
    app: apps[0].id,
    conversation: talk?.id,
  }
}

async function main() {
  const { values } = parseArgs({
    options: {
      out: { type: 'string' },
      only: { type: 'string' },
      list: { type: 'boolean', default: false },
      strict: { type: 'boolean', default: false },
      'unsafe-agent': { type: 'boolean', default: false },
    },
  })
  const only = (values.only ?? process.env.TASKTO_SCREENS_ONLY ?? '')
    .split(',')
    .map((t) => t.trim())
    .filter(Boolean)
  const plan = captures().filter((c) => only.length === 0 || only.some((t) => c.id.includes(t)))
  if (values.list) {
    for (const c of plan) console.log(c.id)
    return 0
  }
  if (plan.length === 0) {
    console.error(`no capture's name contains ${only.map((t) => `"${t}"`).join(' or ')}: see --list`)
    return 1
  }
  const outDir = resolve(values.out || process.env.TASKTO_SCREENS_OUT || join(repo, 'web/screens'))
  const unsafeAgent = values['unsafe-agent'] || process.env.TASKTO_SCREENS_UNSAFE_AGENT === '1'
  const started = Date.now()
  const say = (msg) => console.log(msg)

  // Left out of the module's top so that listing and testing the screens needs no browser.
  const { chromium, request } = await import('playwright')

  const dir = mkdtempSync(join(tmpdir(), 'tt-screens-'))
  let server
  let browser
  let apiContext
  const failures = []
  const issues = new Set()
  const skipped = []
  let done = 0
  try {
    server = await startServer(dir, { unsafeAgent })
    say(`server up (agent runtime ${server.agent})`)
    browser = await chromium.launch()

    // A capture is a page in a context of its size and theme; the contexts share the owner's
    // session once there is one.
    const contexts = new Map()
    const contextFor = async ({ size, theme, dir: name }, storageState) => {
      const key = `${name}:${storageState ? 'in' : 'out'}`
      if (!contexts.has(key)) {
        const context = await browser.newContext({
          baseURL: server.base,
          viewport: { width: size.width, height: size.height },
          colorScheme: theme,
          locale: 'en-US',
          timezoneId: 'UTC',
          storageState,
        })
        context.setDefaultTimeout(15_000)
        contexts.set(key, context)
      }
      return contexts.get(key)
    }

    // What the browser reported, with the capture it happened in. A signed-out screen's calls
    // for the account get 401: that is the app asking who is there, not a fault.
    const watch = (page, phase, id) => {
      const quiet = phase === 'first-run' || phase === 'signed-out'
      const note = (text) => issues.add(`${id}: ${text}`)
      page.on('console', (m) => {
        if (m.type() !== 'error') return
        if (quiet && /status of 401/.test(m.text())) return
        note(`console: ${m.text().slice(0, 300)}`)
      })
      page.on('pageerror', (e) => note(`page error: ${e.message.slice(0, 300)}`))
      page.on('requestfailed', (r) => {
        // A request cut off by the next navigation, or a stream closed with its page, is no fault.
        if (/ERR_ABORTED/.test(r.failure()?.errorText ?? '')) return
        note(`request failed: ${r.method()} ${new URL(r.url()).pathname} ${r.failure()?.errorText ?? ''}`)
      })
      page.on('response', (r) => {
        if (r.status() < 400 || (quiet && r.status() === 401)) return
        note(`HTTP ${r.status()}: ${r.request().method()} ${new URL(r.url()).pathname}`)
      })
    }

    const capture = async (c, storageState, ids, workspace) => {
      const context = await contextFor(c, storageState)
      const page = await context.newPage()
      watch(page, c.screen.phase, c.id)
      const s = {
        page,
        ids,
        path: (section, item) =>
          `/w/${workspace}/${section === 'inbox' ? 'notifications' : section}${item ? `/${encodeURIComponent(item)}` : ''}`,
        go: async (path) => {
          await page.goto(path, { waitUntil: 'networkidle' })
        },
      }
      try {
        await c.screen.show(s)
        // Fonts and a settled layout, with the animations stopped, so two runs match.
        await page.evaluate(() => document.fonts.ready)
        await page.waitForTimeout(400)
        mkdirSync(join(outDir, c.dir), { recursive: true })
        await page.screenshot({ path: join(outDir, `${c.id}.png`), animations: 'disabled', caret: 'hide' })
        done++
      } catch (err) {
        failures.push(`${c.id}: ${String(err.message).split('\n')[0]}`)
      } finally {
        await page.close()
      }
    }

    // Each phase's captures run side by side, one per size and theme.
    const runPhase = async (phase, storageState, ids, workspace) => {
      const todo = plan.filter((c) => c.screen.phase === phase)
      for (const c of todo) if (c.screen.needsAgent && !server.agent.startsWith('on')) skipped.push(c.id)
      const byScreen = Map.groupBy(
        todo.filter((c) => !skipped.includes(c.id)),
        (c) => c.screen.name,
      )
      for (const group of byScreen.values()) {
        await Promise.all(group.map((c) => capture(c, storageState, ids, workspace)))
      }
    }

    // A full run replaces the last one's files; a filtered run leaves the others alone.
    if (only.length === 0) {
      for (const c of plan) {
        const d = join(outDir, c.dir)
        if (existsSync(d)) for (const f of readdirSync(d)) if (f.endsWith('.png')) rmSync(join(d, f))
      }
    }

    await runPhase('first-run', undefined)

    // The owner, through the API: the same session goes to every browser context.
    apiContext = await request.newContext({ baseURL: server.base })
    const api = async (method, path, body) => {
      const res = await apiContext.fetch(path, {
        method,
        headers: { 'Content-Type': 'application/json' },
        data: body === undefined ? undefined : JSON.stringify(body),
      })
      if (!res.ok()) throw new Error(`${method} ${path}: ${res.status()} ${redact((await res.text()).slice(0, 200))}`)
      return res.status() === 204 ? null : res.json()
    }
    const me = await api('POST', '/api/setup', {
      email: owner.email,
      name: owner.name,
      password: owner.password,
      workspace_name: owner.workspace,
      setup_token: server.token,
    })
    const workspace = me.workspaces[0].id
    const session = await apiContext.storageState()

    await runPhase('empty', session, {}, workspace)
    const needsSeed = plan.some((c) => c.screen.phase === 'seeded')
    if (needsSeed) {
      say('seeding the sample data')
      const ids = await seed({ api, workspace, agent: server.agent, say })
      await runPhase('seeded', session, ids, workspace)
    }
    await runPhase('signed-out', undefined)
  } catch (err) {
    failures.push(`the run stopped: ${redact(String(err.message))}`)
    if (server?.logs)
      console.error(`server log (last lines):\n${server.logs().trim().split('\n').slice(-15).join('\n')}`)
  } finally {
    // Whatever happened, the browser and the server stop and the data directory goes.
    await apiContext?.dispose().catch(() => {})
    await browser?.close().catch(() => {})
    await server?.stop().catch(() => {})
    rmSync(dir, { recursive: true, force: true })
  }

  const seconds = Math.round((Date.now() - started) / 1000)
  say(`${done} of ${plan.length - skipped.length} captures written to ${outDir} in ${seconds} s`)
  if (skipped.length > 0)
    say(
      `${skipped.length} skipped, they need the agent runtime: ${[...new Set(skipped.map((id) => id.split('/')[1]))].join(', ')}`,
    )
  if (server?.agent && !server.agent.startsWith('on')) {
    say(
      `note: the agent runtime was ${server.agent}; set TASKTO_SCREENS_UNSAFE_AGENT=1 on a development machine for the conversation's replies`,
    )
  }
  if (issues.size > 0) {
    say(`\nthe browser reported ${issues.size} problem${issues.size === 1 ? '' : 's'}:`)
    for (const i of [...issues].sort()) say(`  ${i}`)
  } else {
    say('the browser reported no console error and no failed request')
  }
  if (failures.length > 0) {
    say(`\n${failures.length} could not be captured:`)
    for (const f of failures) say(`  ${f}`)
  }
  return failures.length > 0 || (values.strict && issues.size > 0) ? 1 : 0
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exitCode = await main()
}
