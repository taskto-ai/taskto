// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { CliAccount, CliAccounts } from '../api'
import { parseRoute, usePath } from '../router'
import { LOGIN_POLL_MS } from '../settings/CliAccountsTab'
import { type Call, fakeFetch, fakeStreams } from '../test/fakes'
import { QUESTIONS_POLL_MS } from './CliPermissions'
import { ConversationsPage } from './ConversationsPage'
import { reconnectAccount } from './ReconnectBanner'
import { pendingHelp, transcript } from './transcript'

const conversation = {
  id: 7,
  workspace_id: 1,
  title: 'On Claude',
  archived_at: null,
  created_at: '2026-11-18T10:00:00.000Z',
  updated_at: '2026-11-18T10:00:00.000Z',
}

beforeEach(() => window.history.replaceState(null, '', '/w/1/conversations/7'))
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

function Page({ streams }: { streams: ReturnType<typeof fakeStreams>['factory'] }) {
  const route = parseRoute(usePath())
  return <ConversationsPage workspace={1} open={route?.item} streams={streams} />
}

const account = (cli: CliAccount['cli'], name: string, state: CliAccount['state']): CliAccount => ({
  id: cli,
  cli,
  slot: 1,
  label: '',
  name,
  available: true,
  state,
  auth_method: '',
  limits: [],
  limits_at: null,
  login: null,
})

const accounts = (...list: CliAccount[]): CliAccounts => ({ enabled: true, mode: 'local', accounts: list })

const MODELS = {
  body: {
    providers: [
      { provider: 'script', name: 'Scripted', scope: 'instance', models: [{ id: 'script-1', name: 'Scripted' }] },
      {
        provider: 'cli-claude',
        name: 'Claude Code',
        scope: 'cli',
        models: [
          { id: 'default', name: 'its default model' },
          { id: 'opus', name: 'opus' },
        ],
      },
      { provider: 'cli-codex', name: 'Codex', scope: 'cli', models: [{ id: 'default', name: 'its default model' }] },
      { provider: 'u2-home', name: 'My Ollama', scope: 'user', models: [{ id: 'llama', name: 'Llama' }] },
    ],
  },
}

const cliEntries = [
  { id: 1, kind: 'pi.user', model: [{ role: 'user', content: 'Fix the tests' }] },
  {
    id: 2,
    kind: 'pi.assistant',
    model: [
      {
        role: 'assistant',
        content: [{ type: 'toolCall', id: 'call-1', name: 'cli_turn', arguments: { cli: 'claude', model: 'default' } }],
        stopReason: 'toolUse',
      },
    ],
  },
]

const running = (output: string) => ({
  type: 'value',
  value: {
    entries: cliEntries,
    docs: {
      'pi.live': {
        run: { taskId: 3 },
        tools: [{ callId: 'call-1', name: 'cli_turn', status: 'running', output }],
      },
    },
  },
})

async function open(routes: Parameters<typeof fakeFetch>[0], view: unknown) {
  const calls = fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [conversation] } },
    'GET /api/workspaces/1/conversations/7': { body: conversation },
    ...routes,
  })
  const { sources, factory } = fakeStreams()
  render(<Page streams={factory} />)
  await waitFor(() => expect(sources).toHaveLength(1))
  act(() => {
    sources[0].open()
    sources[0].send(view)
  })
  return { calls, es: sources[0] }
}

const count = (calls: Call[], method: string, path: string) =>
  calls.filter((c) => c.method === method && c.path === path).length

test('the banner shows only for a conversation on a CLI whose login needs signing in again', () => {
  const list = accounts(account('claude', 'Claude Code', 'reconnect'), account('codex', 'Codex', 'connected'))
  expect(reconnectAccount({ provider: 'cli-claude', model_id: 'default' }, list)?.cli).toBe('claude')
  expect(reconnectAccount({ provider: 'cli-codex', model_id: 'default' }, list)).toBeNull()
  expect(reconnectAccount({ provider: 'cli-grok', model_id: 'default' }, list)).toBeNull()
  expect(reconnectAccount({ provider: 'script', model_id: 'script-1' }, list)).toBeNull()
  expect(reconnectAccount({ provider: 'u2-cli-claude', model_id: 'x' }, list)).toBeNull()
  expect(reconnectAccount(undefined, list)).toBeNull()
  expect(reconnectAccount({ provider: 'cli-claude', model_id: 'default' }, null)).toBeNull()
  expect(reconnectAccount({ provider: 'cli-claude', model_id: 'default' }, { ...list, enabled: false })).toBeNull()
})

test("no banner while another of the user's accounts for the CLI is connected: the turn runs there", () => {
  const second = { ...account('claude', 'Claude Code', 'connected'), id: 'claude-2', slot: 2 }
  const both = accounts(account('claude', 'Claude Code', 'reconnect'), second)
  expect(reconnectAccount({ provider: 'cli-claude', model_id: 'default' }, both)).toBeNull()
  const neither = accounts(account('claude', 'Claude Code', 'reconnect'), { ...second, state: 'reconnect' })
  expect(reconnectAccount({ provider: 'cli-claude', model_id: 'default' }, neither)?.id).toBe('claude')
})

test("a CLI's turn shows as that CLI's work, its output as plain text", async () => {
  const output = 'Looking at the tests\n▸ Bash npm test\n<img src=x onerror=alert(1)> **not bold**\n'
  await open(
    {
      'GET /api/workspaces/1/models': MODELS,
      'GET /api/workspaces/1/conversations/7/model': { body: { provider: 'cli-claude', model_id: 'default' } },
      'GET /api/me/cli-accounts': { body: accounts(account('claude', 'Claude Code', 'connected')) },
    },
    running(output),
  )
  expect(screen.getByText('Claude Code is working')).toBeTruthy()
  expect(screen.queryByText(/Used cli_turn/)).toBeNull()
  const pre = document.querySelector('pre')
  expect(pre?.textContent).toBe(output)
  expect(pre?.querySelector('img, strong')).toBeNull()
  expect(pre?.closest('details')?.open).toBe(true)
  expect(document.querySelector('img')).toBeNull()
  expect(screen.queryByRole('region', { name: /Reconnect/ })).toBeNull()
})

test('a finished CLI turn says the CLI worked', () => {
  const t = transcript({
    entries: [
      ...cliEntries,
      {
        id: 3,
        kind: 'pi.toolResult',
        model: [
          { role: 'toolResult', toolCallId: 'call-1', toolName: 'cli_turn', content: [{ type: 'text', text: 'Done' }] },
        ],
      },
      { id: 4, kind: 'pi.assistant', model: [{ role: 'assistant', content: [{ type: 'text', text: 'Done' }] }] },
    ],
    docs: { 'pi.live': {} },
  })
  expect(t.messages[1].tools).toEqual([{ id: 'call-1', key: '2.0.0', name: 'cli_turn', cli: 'claude' }])
})

test('a finished CLI turn keeps showing what the CLI did, from its result', () => {
  const kept = '▸ Bash echo hi\nhi\nDone'
  const t = transcript({
    entries: [
      ...cliEntries,
      {
        id: 3,
        kind: 'pi.toolResult',
        model: [
          {
            role: 'toolResult',
            toolCallId: 'call-1',
            toolName: 'cli_turn',
            content: [{ type: 'text', text: 'Done' }],
            details: { transcript: kept },
          },
        ],
      },
      { id: 4, kind: 'pi.assistant', model: [{ role: 'assistant', content: [{ type: 'text', text: 'Done' }] }] },
    ],
    docs: { 'pi.live': {} },
  })
  expect(t.messages[1].tools).toEqual([{ id: 'call-1', key: '2.0.0', name: 'cli_turn', cli: 'claude', output: kept }])
})

test('a turn waiting for a new login shows the banner; Reconnect signs in right there and it goes', async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  let state: CliAccount['state'] = 'connected'
  let loginState = 'waiting'
  const { calls, es } = await open(
    {
      'GET /api/workspaces/1/models': MODELS,
      'GET /api/workspaces/1/conversations/7/model': { body: { provider: 'cli-claude', model_id: 'default' } },
      'GET /api/me/cli-accounts': () => ({ body: accounts(account('claude', 'Claude Code', state)) }),
      'POST /api/me/cli-accounts/claude/login': {
        body: {
          cli: 'claude',
          state: 'waiting',
          url: 'https://claude.ai/oauth/authorize?code=true',
          url_trusted: true,
          needs_code: true,
          started_at: '2026-11-19T15:10:00Z',
        },
      },
      'GET /api/me/cli-accounts/claude/login': () => {
        if (loginState === 'connected') state = 'connected'
        return {
          body: {
            cli: 'claude',
            state: loginState,
            url_trusted: true,
            needs_code: true,
            started_at: '2026-11-19T15:10:00Z',
          },
        }
      },
      'POST /api/me/cli-accounts/claude/login/code': {
        status: 202,
        body: {
          cli: 'claude',
          state: 'waiting',
          url_trusted: true,
          needs_code: true,
          started_at: '2026-11-19T15:10:00Z',
        },
      },
    },
    running('Looking at the tests\n'),
  )
  await screen.findByLabelText('Model')
  await waitFor(() => expect(count(calls, 'GET', '/api/me/cli-accounts')).toBeGreaterThan(0))
  const before = count(calls, 'GET', '/api/me/cli-accounts')
  expect(screen.queryByRole('region', { name: 'Reconnect Claude Code' })).toBeNull()

  state = 'reconnect'
  act(() =>
    es.send(
      running(
        'Looking at the tests\n⚠ Claude Code needs you to sign in again: reconnect it (Settings → CLI accounts).\n',
      ),
    ),
  )
  const banner = await screen.findByRole('region', { name: 'Reconnect Claude Code' })
  expect(count(calls, 'GET', '/api/me/cli-accounts')).toBe(before + 1)
  expect(
    within(banner).getByText('Claude Code needs you to sign in again — this conversation continues once you have.'),
  ).toBeTruthy()

  fireEvent.click(within(banner).getByRole('button', { name: 'Reconnect Claude Code' }))
  const link = await within(banner).findByRole('link', { name: 'https://claude.ai/oauth/authorize?code=true' })
  expect(link.getAttribute('target')).toBe('_blank')
  fireEvent.change(within(banner).getByLabelText('Paste the code the sign-in page shows'), {
    target: { value: 'fake-code' },
  })
  fireEvent.click(within(banner).getByRole('button', { name: 'Send the code' }))
  await waitFor(() => expect(count(calls, 'POST', '/api/me/cli-accounts/claude/login/code')).toBe(1))

  loginState = 'connected'
  await act(() => vi.advanceTimersByTimeAsync(LOGIN_POLL_MS))
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Reconnect Claude Code' })).toBeNull())
})

test('CLI models are their own group; one not connected is still offered, with a hint', async () => {
  const { calls } = await open(
    {
      'GET /api/workspaces/1/models': MODELS,
      'GET /api/workspaces/1/conversations/7/model': { body: { provider: 'script', model_id: 'script-1' } },
      'GET /api/me/cli-accounts': {
        body: accounts(account('claude', 'Claude Code', 'connected'), account('codex', 'Codex', 'disconnected')),
      },
      'PUT /api/workspaces/1/conversations/7/model': (call) => ({ body: call.body }),
    },
    { type: 'value', value: { entries: [], docs: { 'pi.live': {} } } },
  )
  const select = (await screen.findByLabelText('Model')) as HTMLSelectElement
  await waitFor(() =>
    expect(within(select).getByRole('option', { name: 'Codex: its default model (not connected)' })).toBeTruthy(),
  )
  const groups = [...select.querySelectorAll('optgroup')].map((g) => [
    g.label,
    [...g.querySelectorAll('option')].map((o) => o.textContent),
  ])
  expect(groups).toEqual([
    ['Scripted', ['Scripted']],
    [
      'CLI workers (your own subscription)',
      ['Claude Code: its default model', 'Claude Code: opus', 'Codex: its default model (not connected)'],
    ],
    ['My Ollama', ['Llama']],
  ])
  expect(screen.queryByText(/isn't connected/)).toBeNull()

  fireEvent.change(select, { target: { value: JSON.stringify(['cli-codex', 'default']) } })
  await waitFor(() =>
    expect(calls.filter((c) => c.method === 'PUT').map((c) => c.body)).toEqual([
      { provider: 'cli-codex', model_id: 'default' },
    ]),
  )
  const hint = await screen.findByText(/Codex isn't connected: connect it in/)
  expect(within(hint).getByRole('link', { name: 'Settings → CLI accounts' }).getAttribute('href')).toBe(
    '/w/1/settings/cli-accounts',
  )
  expect(screen.queryByRole('region', { name: /Reconnect/ })).toBeNull()
})

test('without CLI models, the CLI accounts are never asked for', async () => {
  const { calls } = await open(
    {
      'GET /api/workspaces/1/models': {
        body: {
          providers: [
            { provider: 'script', name: 'Scripted', scope: 'instance', models: [{ id: 'script-1', name: 'Scripted' }] },
          ],
        },
      },
      'GET /api/workspaces/1/conversations/7/model': { body: { provider: 'script', model_id: 'script-1' } },
    },
    { type: 'value', value: { entries: [], docs: { 'pi.live': {} } } },
  )
  await screen.findByLabelText('Model')
  expect(count(calls, 'GET', '/api/me/cli-accounts')).toBe(0)
})

test("a CLI's question shows with Allow and Deny; the answer goes to the server and the question goes", async () => {
  let waiting = true
  const { calls, es } = await open(
    {
      'GET /api/workspaces/1/models': MODELS,
      'GET /api/workspaces/1/conversations/7/model': { body: { provider: 'cli-claude', model_id: 'default' } },
      'GET /api/me/cli-accounts': { body: accounts(account('claude', 'Claude Code', 'connected')) },
      'GET /api/workspaces/1/conversations/7/cli-permissions': () => ({
        body: {
          requests: waiting
            ? [
                {
                  id: 'q1',
                  turn: 'c7-t1',
                  cli: 'claude',
                  tool: 'Bash',
                  input: 'rm -rf build <b>x</b>',
                  asked_at: '2026-11-19T15:10:00Z',
                },
              ]
            : [],
        },
      }),
      'POST /api/workspaces/1/conversations/7/cli-permissions/q1': () => {
        waiting = false
        return { status: 204 }
      },
    },
    running('Looking at the tests\n'),
  )
  await screen.findByLabelText('Model')
  act(() => es.send(running('Looking at the tests\n? Claude Code asks to use Bash: rm -rf build\n')))
  const question = await screen.findByRole('region', { name: 'Claude Code asks to use Bash' })
  expect(question.textContent).toContain('Claude Code asks to use Bash:')
  expect(question.textContent).not.toContain('{tool}')
  expect(within(question).getByText('Bash').tagName).toBe('STRONG')
  expect(within(question).getByText('rm -rf build <b>x</b>').tagName).toBe('PRE')
  fireEvent.click(within(question).getByRole('button', { name: 'Deny' }))
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Claude Code asks to use Bash' })).toBeNull())
  const answers = calls.filter((c) => c.method === 'POST' && c.path.endsWith('/cli-permissions/q1'))
  expect(answers.map((c) => c.body)).toEqual([{ allow: false }])
})

test('allowing everything in a conversation asks first, and only for that conversation', async () => {
  let perms = 'ask'
  const { calls } = await open(
    {
      'GET /api/workspaces/1/conversations/7': () => ({ body: { ...conversation, cli_permissions: perms } }),
      'PATCH /api/workspaces/1/conversations/7': (call) => {
        perms = (call.body as { cli_permissions: string }).cli_permissions
        return { body: { ...conversation, cli_permissions: perms } }
      },
      'GET /api/workspaces/1/models': MODELS,
      'GET /api/workspaces/1/conversations/7/model': { body: { provider: 'cli-codex', model_id: 'default' } },
      'GET /api/me/cli-accounts': { body: accounts(account('codex', 'Codex', 'connected')) },
      'GET /api/workspaces/1/conversations/7/cli-permissions': { body: { requests: [] } },
    },
    running(''),
  )
  const box = (await screen.findByLabelText('Let Codex act without asking in this conversation')) as HTMLInputElement
  expect(box.checked).toBe(false)

  const ask = () => screen.findByRole('alertdialog', { name: 'Let Codex act without asking?' })
  fireEvent.click(box)
  const question = await ask()
  expect(question.textContent).toContain('without asking you first')
  fireEvent.click(within(question).getByRole('button', { name: 'Cancel' }))
  await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
  expect(box.checked).toBe(false)
  expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(0)

  fireEvent.click(box)
  fireEvent.click(within(await ask()).getByRole('button', { name: 'Allow everything' }))
  await waitFor(() => expect(box.checked).toBe(true))
  expect(calls.filter((c) => c.method === 'PATCH').map((c) => c.body)).toEqual([{ cli_permissions: 'allow_all' }])

  fireEvent.click(box)
  await waitFor(() => expect(box.checked).toBe(false))
  expect(screen.queryByRole('alertdialog')).toBeNull()
  expect(calls.filter((c) => c.method === 'PATCH').map((c) => c.body)).toEqual([
    { cli_permissions: 'allow_all' },
    { cli_permissions: 'ask' },
  ])
})

test("a CLI's questions are read again once for each new question line, and not for other output", async () => {
  const { calls, es } = await open(
    {
      'GET /api/workspaces/1/models': MODELS,
      'GET /api/workspaces/1/conversations/7/model': { body: { provider: 'cli-claude', model_id: 'default' } },
      'GET /api/me/cli-accounts': { body: accounts(account('claude', 'Claude Code', 'connected')) },
      'GET /api/workspaces/1/conversations/7/cli-permissions': { body: { requests: [] } },
    },
    running('Looking at the tests\n'),
  )
  await screen.findByLabelText('Model')
  const reads = () => count(calls, 'GET', '/api/workspaces/1/conversations/7/cli-permissions')
  await waitFor(() => expect(reads()).toBeGreaterThan(0))
  await new Promise((done) => setTimeout(done, 50))
  const before = reads()
  act(() => es.send(running('Looking at the tests\n? Claude Code asks to use Bash: ls\n')))
  await waitFor(() => expect(reads()).toBe(before + 1))
  act(() => es.send(running('Looking at the tests\n? Claude Code asks to use Bash: ls\nStill looking\n')))
  act(() => es.send(running('Looking at the tests\n? Claude Code asks to use Bash: ls\nStill looking\n  ✓ Allowed\n')))
  await waitFor(() => expect(reads()).toBe(before + 2))
  await new Promise((done) => setTimeout(done, 50))
  expect(reads()).toBe(before + 2)
})

test('while a CLI works, its questions are read again now and then, even without a line saying so', async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  const { calls } = await open(
    {
      'GET /api/workspaces/1/models': MODELS,
      'GET /api/workspaces/1/conversations/7/model': { body: { provider: 'cli-claude', model_id: 'default' } },
      'GET /api/me/cli-accounts': { body: accounts(account('claude', 'Claude Code', 'connected')) },
      'GET /api/workspaces/1/conversations/7/cli-permissions': { body: { requests: [] } },
    },
    running('Working\n'),
  )
  await screen.findByLabelText('Model')
  const reads = () => count(calls, 'GET', '/api/workspaces/1/conversations/7/cli-permissions')
  await waitFor(() => expect(reads()).toBeGreaterThan(0))
  const before = reads()
  await act(() => vi.advanceTimersByTimeAsync(QUESTIONS_POLL_MS))
  expect(reads()).toBeGreaterThan(before)
})

test("a CLI's request to take over the browser is shown as the built-in agent's", () => {
  const help = { reason: 'login', message: 'Sign in to the shop, then press Done.' }
  const result = (details: unknown) =>
    transcript({
      entries: [
        ...cliEntries,
        {
          id: 3,
          kind: 'pi.toolResult',
          model: [
            {
              role: 'toolResult',
              toolCallId: 'call-1',
              toolName: 'cli_turn',
              content: [{ type: 'text', text: 'Waiting for you.' }],
              details,
            },
          ],
        },
        {
          id: 4,
          kind: 'pi.assistant',
          model: [{ role: 'assistant', content: [{ type: 'text', text: 'Waiting for you.' }] }],
        },
      ],
      docs: { 'pi.live': {} },
    })
  const t = result({ transcript: '▸ mcp__taskto__browser_help', help })
  expect(t.messages[1].tools).toEqual([
    { id: 'call-1', key: '2.0.0', name: 'cli_turn', cli: 'claude', output: '▸ mcp__taskto__browser_help', help },
  ])
  expect(pendingHelp(t.messages)).toEqual(help)

  expect(pendingHelp(result({ help: { reason: 'login', message: '  ' } }).messages)).toBeNull()
  expect(pendingHelp(result({ help: 'sign in' }).messages)).toBeNull()

  const answered = result({ help }).messages
  answered.push({ key: '5.0', role: 'user', text: 'Done', tools: [] })
  expect(pendingHelp(answered)).toBeNull()
})

const done = (kept: string) => ({
  type: 'value',
  value: {
    entries: [
      ...cliEntries,
      {
        id: 3,
        kind: 'pi.toolResult',
        model: [
          {
            role: 'toolResult',
            toolCallId: 'call-1',
            toolName: 'cli_turn',
            content: [{ type: 'text', text: 'Done' }],
            details: { transcript: kept },
          },
        ],
      },
    ],
    docs: { 'pi.live': {} },
  },
})

const cliRoutes = {
  'GET /api/workspaces/1/models': MODELS,
  'GET /api/workspaces/1/conversations/7/model': { body: { provider: 'cli-claude', model_id: 'default' } },
  'GET /api/me/cli-accounts': { body: accounts(account('claude', 'Claude Code', 'connected')) },
}

test("a CLI's output is open while it works and folds once it is done", async () => {
  const { es } = await open(cliRoutes, running('$ go test\n'))
  const details = () => document.querySelector('pre')?.closest('details') as HTMLDetailsElement
  expect(details().open).toBe(true)
  act(() => es.send(done('$ go test\nok')))
  await waitFor(() => expect(screen.getByText('Claude Code worked')).toBeTruthy())
  expect(details().open).toBe(false)
  expect(document.querySelector('pre')?.textContent).toBe('$ go test\nok')
})

test('output the person scrolled in stays open when the CLI is done', async () => {
  const { es } = await open(cliRoutes, running('$ go test\n'))
  const pre = document.querySelector('pre') as HTMLElement
  fireEvent.scroll(pre)
  act(() => es.send(done('$ go test\nok')))
  await waitFor(() => expect(screen.getByText('Claude Code worked')).toBeTruthy())
  expect(pre.closest('details')?.open).toBe(true)
})

test('the answer on its way shows on its own button', async () => {
  await open(
    {
      ...cliRoutes,
      'GET /api/workspaces/1/conversations/7/cli-permissions': {
        body: { requests: [{ id: 'q1', turn: 't1', cli: 'claude', tool: 'Bash', asked_at: '2026-11-19T15:10:00Z' }] },
      },
      'POST /api/workspaces/1/conversations/7/cli-permissions/q1': () => new Promise(() => {}),
    },
    running(''),
  )
  const question = await screen.findByRole('region', { name: 'Claude Code asks to use Bash' })
  fireEvent.click(within(question).getByRole('button', { name: 'Deny' }))
  await waitFor(() =>
    expect(within(question).getByRole('button', { name: /^Deny/ }).getAttribute('aria-busy')).toBe('true'),
  )
  const allow = within(question).getByRole('button', { name: 'Allow' }) as HTMLButtonElement
  expect(allow.getAttribute('aria-busy')).toBeNull()
  expect(allow.disabled).toBe(true)
})

const refOf = (provider: string, model_id: string) => JSON.stringify([provider, model_id])
const modelPath = '/api/workspaces/1/conversations/7/model'

type ModelRead = { body: unknown } | { status: number; body: unknown } | Promise<{ body: unknown }>

function modelServer(first: { provider: string; model_id: string }) {
  const state = {
    server: first,
    reads: 0,
    plan: (_n: number): ModelRead | undefined => undefined,
  }
  const routes = {
    ...cliRoutes,
    'GET /api/workspaces/1/conversations/7/model': () => {
      state.reads++
      return state.plan(state.reads) ?? { body: state.server }
    },
    'PUT /api/workspaces/1/conversations/7/model': (call: Call) => {
      state.server = call.body as typeof first
      return { body: call.body }
    },
  }
  return { state, routes }
}

async function pickOpus() {
  const select = (await screen.findByLabelText('Model')) as HTMLSelectElement
  await waitFor(() => expect(select.value).toBe(refOf('cli-claude', 'default')))
  fireEvent.change(select, { target: { value: refOf('cli-claude', 'opus') } })
  await waitFor(() => expect(select.value).toBe(refOf('cli-claude', 'opus')))
  return select
}

test('the model the server moved a conversation to after a limit replaces the one shown, also a picked one', async () => {
  const { state, routes } = modelServer({ provider: 'cli-claude', model_id: 'default' })
  const { calls, es } = await open(routes, running('$ go test\n'))
  const select = await pickOpus()
  state.server = { provider: 'u2-home', model_id: 'llama' }
  const reads = count(calls, 'GET', modelPath)
  act(() => es.send(done('$ go test\nok')))
  await waitFor(() => expect(count(calls, 'GET', modelPath)).toBe(reads + 1))
  await waitFor(() => expect(select.value).toBe(refOf('u2-home', 'llama')))
})

test('a read that started before the pick, and ends after it, does not undo the pick', async () => {
  const { state, routes } = modelServer({ provider: 'cli-claude', model_id: 'default' })
  let hold: ((v: { body: unknown }) => void) | null = null
  state.plan = (n) => (n === 2 ? new Promise((resolve) => (hold = resolve)) : undefined)
  const { es } = await open(routes, running('$ go test\n'))
  const select = (await screen.findByLabelText('Model')) as HTMLSelectElement
  await waitFor(() => expect(select.value).toBe(refOf('cli-claude', 'default')))
  act(() => es.send(done('$ go test\nok')))
  await waitFor(() => expect(state.reads).toBe(2))
  fireEvent.change(select, { target: { value: refOf('cli-claude', 'opus') } })
  await waitFor(() => expect(select.value).toBe(refOf('cli-claude', 'opus')))
  await act(async () => hold?.({ body: { provider: 'cli-claude', model_id: 'default' } }))
  await waitFor(() => expect(select.value).toBe(refOf('cli-claude', 'opus')))
  expect(select.value).toBe(refOf('cli-claude', 'opus'))
})

test('a failed or a pending read after a pick keeps the pick', async () => {
  const { state, routes } = modelServer({ provider: 'cli-claude', model_id: 'default' })
  let hold: ((v: { body: unknown }) => void) | null = null
  state.plan = (n) => {
    if (n === 2) return { status: 500, body: { error: 'down' } }
    if (n === 3) return new Promise((resolve) => (hold = resolve))
    return undefined
  }
  const { es } = await open(routes, running('$ go test\n'))
  const select = await pickOpus()
  act(() => es.send(done('$ go test\nok')))
  await waitFor(() => expect(state.reads).toBe(2))
  await new Promise((resolve) => setTimeout(resolve, 50))
  expect(select.value).toBe(refOf('cli-claude', 'opus'))
  act(() => es.send(running('')))
  act(() => es.send(done('$ go test\nok again')))
  await waitFor(() => expect(state.reads).toBe(3))
  expect(select.value).toBe(refOf('cli-claude', 'opus'))
  await act(async () => hold?.({ body: state.server }))
  await waitFor(() => expect(select.value).toBe(refOf('cli-claude', 'opus')))
})

test('once a newer read has the server model, a pick made earlier is gone: a later failed read keeps the server model', async () => {
  const { state, routes } = modelServer({ provider: 'cli-claude', model_id: 'default' })
  state.plan = (n) => (n === 3 ? { status: 500, body: { error: 'down' } } : undefined)
  const { es } = await open(routes, running('$ go test\n'))
  const select = await pickOpus()
  state.server = { provider: 'u2-home', model_id: 'llama' }
  act(() => es.send(done('$ go test\nok')))
  await waitFor(() => expect(select.value).toBe(refOf('u2-home', 'llama')))
  act(() => es.send(running('')))
  act(() => es.send(done('$ go test\nok again')))
  await waitFor(() => expect(state.reads).toBe(3))
  await new Promise((resolve) => setTimeout(resolve, 50))
  expect(select.value).toBe(refOf('u2-home', 'llama'))
})

test('a turn finished while the stream was down, seen only in a new snapshot, refreshes the model shown', async () => {
  const { state, routes } = modelServer({ provider: 'cli-claude', model_id: 'default' })
  const idle = (extra: unknown[]) => ({
    type: 'value',
    value: { entries: [...cliEntries, ...extra], docs: { 'pi.live': {} } },
  })
  const { es } = await open(routes, idle([]))
  const select = (await screen.findByLabelText('Model')) as HTMLSelectElement
  await waitFor(() => expect(select.value).toBe(refOf('cli-claude', 'default')))
  state.server = { provider: 'u2-home', model_id: 'llama' }
  act(() =>
    es.send(
      idle([
        {
          id: 3,
          kind: 'pi.toolResult',
          model: [
            { role: 'toolResult', toolCallId: 'call-1', toolName: 'cli_turn', content: [{ type: 'text', text: 'x' }] },
          ],
        },
        { id: 4, kind: 'pi.assistant', model: [{ role: 'assistant', content: [{ type: 'text', text: 'Answered' }] }] },
      ]),
    ),
  )
  await waitFor(() => expect(select.value).toBe(refOf('u2-home', 'llama')))
})

test('a read that a newer one replaced does not retire the pick when it ends', async () => {
  const { state, routes } = modelServer({ provider: 'cli-claude', model_id: 'default' })
  let hold: ((v: { body: unknown }) => void) | null = null
  state.plan = (n) => {
    if (n === 2) return new Promise((resolve) => (hold = resolve))
    if (n === 3) return { status: 500, body: { error: 'down' } }
    return undefined
  }
  const { es } = await open(routes, running('$ go test\n'))
  const select = await pickOpus()
  act(() => es.send(done('$ go test\nok')))
  await waitFor(() => expect(state.reads).toBe(2))
  act(() => es.send(running('')))
  act(() => es.send(done('$ go test\nok again')))
  await waitFor(() => expect(state.reads).toBe(3))
  await act(async () => hold?.({ body: { provider: 'cli-claude', model_id: 'default' } }))
  await new Promise((resolve) => setTimeout(resolve, 50))
  expect(select.value).toBe(refOf('cli-claude', 'opus'))
})
