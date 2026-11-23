// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { createRoot } from 'react-dom/client'
import { afterEach, expect, test, vi } from 'vitest'
import type { CliAccount, CliAccounts, CliLogin } from '../api'
import { format } from '../i18n'
import { type Call, fakeFetch } from '../test/fakes'
import { CliAccountsTab, CliLimits, LOGIN_POLL_MS, windowLabel } from './CliAccountsTab'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

const account = (cli: CliAccount['cli'], name: string, extra: Partial<CliAccount> = {}): CliAccount => ({
  id: cli,
  cli,
  slot: 1,
  label: '',
  name,
  available: true,
  state: 'disconnected',
  auth_method: '',
  limits: [],
  limits_at: null,
  login: null,
  ...extra,
})

// fakeFetch with the switching queue's route, which the tab reads too.
const fakeCli = (routes: Parameters<typeof fakeFetch>[0]) =>
  fakeFetch({ 'GET /api/me/cli-switching': { body: { enabled: false, queue: [], available: [] } }, ...routes })

const list = (...accounts: CliAccount[]): { body: CliAccounts } => ({
  body: { enabled: true, mode: 'local', accounts },
})

const login = (cli: CliLogin['cli'], extra: Partial<CliLogin> = {}): CliLogin => ({
  cli,
  state: 'waiting',
  url_trusted: true,
  needs_code: false,
  started_at: '2026-11-19T15:10:00Z',
  ...extra,
})

const count = (calls: Call[], method: string, path: string) =>
  calls.filter((c) => c.method === method && c.path === path).length

const poll = () => act(() => vi.advanceTimersByTimeAsync(LOGIN_POLL_MS))

test('CLI workers that are off say why, with no buttons', async () => {
  fakeCli({
    'GET /api/me/cli-accounts': {
      body: { enabled: false, mode: 'off', reason: 'CLI workers are off on this server', accounts: [] },
    },
  })
  render(<CliAccountsTab />)
  expect(await screen.findByText('CLI workers are off on this server')).toBeTruthy()
  expect(screen.queryByRole('button')).toBeNull()
})

test("the reason's code shows in the user's language, not the server's words", async () => {
  fakeCli({
    'GET /api/me/cli-accounts': {
      body: { enabled: false, mode: 'off', reason: 'in English', reason_code: 'cli_workers_off', accounts: [] },
    },
  })
  render(<CliAccountsTab />)
  expect(await screen.findByText('CLI workers are off on this server.')).toBeTruthy()
  expect(screen.queryByText('in English')).toBeNull()
})

test('one card per CLI: its state, how it signed in, terms next to Connect, and one not installed', async () => {
  fakeCli({
    'GET /api/me/cli-accounts': list(
      account('claude', 'Claude Code', { state: 'connected', auth_method: 'claude.ai' }),
      account('codex', 'Codex', { state: 'reconnect', auth_method: 'chatgpt' }),
      account('grok', 'Grok Build', { available: false }),
    ),
  })
  render(<CliAccountsTab />)
  const claude = await screen.findByRole('region', { name: 'Claude Code' })
  expect(within(claude).getByText('Connected')).toBeTruthy()
  expect(within(claude).getByText(/signed in with claude\.ai/)).toBeTruthy()
  // Connected: no Connect, but Check and Disconnect.
  expect(within(claude).queryByRole('button', { name: 'Connect Claude Code' })).toBeNull()
  expect(within(claude).getByRole('button', { name: 'Check Claude Code' })).toBeTruthy()
  expect(within(claude).getByRole('button', { name: 'Disconnect Claude Code' })).toBeTruthy()
  const terms = within(claude).getByRole('link', { name: "Claude Code's terms" })
  expect(terms.getAttribute('href')).toBe('https://code.claude.com/docs/en/legal-and-compliance')
  expect(terms.getAttribute('rel')).toBe('noopener noreferrer')
  expect(within(claude).getByText(/runs on your own subscription/)).toBeTruthy()

  const codex = screen.getByRole('region', { name: 'Codex' })
  expect(within(codex).getByText('Needs you to sign in again')).toBeTruthy()
  expect(within(codex).getByRole('button', { name: 'Reconnect Codex' })).toBeTruthy()
  expect(within(codex).getByRole('link', { name: "Codex's terms" }).getAttribute('href')).toBe(
    'https://openai.com/policies/terms-of-use/',
  )

  const grok = screen.getByRole('region', { name: 'Grok Build' })
  expect(within(grok).getByText('Not installed on this server.')).toBeTruthy()
  expect(within(grok).queryByRole('button')).toBeNull()
  expect(within(grok).getByText("Grok Build doesn't report usage limits.")).toBeTruthy()
})

test('a trusted sign-in URL is a link, with the one-time code, until the login connects', async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  let state: CliLogin['state'] = 'waiting'
  let connected = false
  const calls = fakeCli({
    'GET /api/me/cli-accounts': () =>
      list(account('codex', 'Codex', connected ? { state: 'connected', auth_method: 'chatgpt' } : {})),
    'POST /api/me/cli-accounts/codex/login': {
      body: login('codex', { url: 'https://auth.openai.com/codex/device', code: 'ABCD-EFGHI' }),
    },
    'GET /api/me/cli-accounts/codex/login': () => {
      if (state === 'connected') connected = true
      return { body: login('codex', { state, url: 'https://auth.openai.com/codex/device', code: 'ABCD-EFGHI' }) }
    },
  })
  render(<CliAccountsTab />)
  fireEvent.click(await screen.findByRole('button', { name: 'Connect Codex' }))

  const link = await screen.findByRole('link', { name: 'https://auth.openai.com/codex/device' })
  expect(link.getAttribute('href')).toBe('https://auth.openai.com/codex/device')
  expect(link.getAttribute('target')).toBe('_blank')
  expect(link.getAttribute('rel')).toBe('noopener noreferrer')
  expect((screen.getByLabelText('One-time code') as HTMLInputElement).value).toBe('ABCD-EFGHI')
  expect(screen.getByRole('button', { name: 'Copy the code' })).toBeTruthy()
  expect(screen.queryByText(/known sign-in page/)).toBeNull()
  expect(screen.queryByLabelText('Paste the code the sign-in page shows')).toBeNull()
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({})

  // Read again every LOGIN_POLL_MS while it waits.
  await poll()
  expect(count(calls, 'GET', '/api/me/cli-accounts/codex/login')).toBe(1)
  await poll()
  expect(count(calls, 'GET', '/api/me/cli-accounts/codex/login')).toBe(2)

  state = 'connected'
  await poll()
  await waitFor(() => expect(screen.getByText('Connected')).toBeTruthy())
  expect(count(calls, 'GET', '/api/me/cli-accounts')).toBe(2)
  expect(screen.queryByRole('link', { name: 'https://auth.openai.com/codex/device' })).toBeNull()
  expect(screen.queryByRole('button', { name: 'Connect Codex' })).toBeNull()
  // Polling stopped.
  await poll()
  expect(count(calls, 'GET', '/api/me/cli-accounts/codex/login')).toBe(3)
})

test("an untrusted URL is text with a warning; Claude Code's code is pasted back", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  const waiting = login('claude', {
    url: 'https://example.com/<b>not</b>-claude',
    url_trusted: false,
    needs_code: true,
  })
  const calls = fakeCli({
    'GET /api/me/cli-accounts': list(account('claude', 'Claude Code')),
    'POST /api/me/cli-accounts/claude/login': { body: waiting },
    'GET /api/me/cli-accounts/claude/login': { body: waiting },
    'POST /api/me/cli-accounts/claude/login/code': { status: 202, body: waiting },
  })
  render(<CliAccountsTab />)
  fireEvent.click(await screen.findByRole('button', { name: 'Connect Claude Code' }))

  const url = await screen.findByText('https://example.com/<b>not</b>-claude')
  expect(url.tagName).toBe('CODE')
  expect(url.closest('a')).toBeNull()
  expect(screen.queryByRole('link', { name: /example\.com/ })).toBeNull()
  expect(document.querySelector('b')).toBeNull()
  expect(
    screen.getByText("This address isn't Claude Code's known sign-in page. Check it before you open it."),
  ).toBeTruthy()

  fireEvent.change(screen.getByLabelText('Paste the code the sign-in page shows'), {
    target: { value: '  fake-code#state  ' },
  })
  fireEvent.click(screen.getByRole('button', { name: 'Send the code' }))
  await waitFor(() =>
    expect(calls.filter((c) => c.path.endsWith('/login/code')).map((c) => c.body)).toEqual([
      { code: 'fake-code#state' },
    ]),
  )
  await waitFor(() =>
    expect((screen.getByLabelText('Paste the code the sign-in page shows') as HTMLInputElement).value).toBe(''),
  )
})

test('a failed login shows why and can be tried again; Cancel ends one', async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  let polled: CliLogin = login('grok', { url: 'https://accounts.x.ai/device', code: 'WXYZ-1234' })
  const calls = fakeCli({
    'GET /api/me/cli-accounts': list(account('grok', 'Grok Build')),
    'POST /api/me/cli-accounts/grok/login': { body: login('grok', { state: 'starting' }) },
    'GET /api/me/cli-accounts/grok/login': () => ({ body: polled }),
    'DELETE /api/me/cli-accounts/grok/login': { status: 204 },
  })
  render(<CliAccountsTab />)
  fireEvent.click(await screen.findByRole('button', { name: 'Connect Grok Build' }))
  expect(await screen.findByText("Starting Grok Build's sign-in…")).toBeTruthy()
  await poll()
  expect(await screen.findByRole('link', { name: 'https://accounts.x.ai/device' })).toBeTruthy()

  polled = login('grok', { state: 'failed', message: 'grok login: the device code was denied' })
  await poll()
  expect((await screen.findByRole('alert')).textContent).toBe(
    'The sign-in failed. grok login: the device code was denied',
  )
  const before = count(calls, 'GET', '/api/me/cli-accounts/grok/login')
  await poll()
  expect(count(calls, 'GET', '/api/me/cli-accounts/grok/login')).toBe(before)

  polled = login('grok', { url: 'https://accounts.x.ai/device', code: 'WXYZ-1234' })
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
  expect(await screen.findByText("Starting Grok Build's sign-in…")).toBeTruthy()
  await poll()
  expect(await screen.findByRole('link', { name: 'https://accounts.x.ai/device' })).toBeTruthy()
  expect(count(calls, 'POST', '/api/me/cli-accounts/grok/login')).toBe(2)

  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(await screen.findByRole('button', { name: 'Connect Grok Build' })).toBeTruthy()
  expect(count(calls, 'DELETE', '/api/me/cli-accounts/grok/login')).toBe(1)
})

test('a login already in progress is picked up from the list, and an expired one says so', async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  fakeCli({
    'GET /api/me/cli-accounts': list(
      account('codex', 'Codex', {
        login: login('codex', { url: 'https://auth.openai.com/codex/device', code: 'AB-CD' }),
      }),
    ),
    'GET /api/me/cli-accounts/codex/login': { body: login('codex', { state: 'expired' }) },
  })
  render(<CliAccountsTab />)
  expect(await screen.findByRole('link', { name: 'https://auth.openai.com/codex/device' })).toBeTruthy()
  await poll()
  expect((await screen.findByRole('alert')).textContent).toBe('The sign-in expired.')
  expect(screen.getByRole('button', { name: 'Try again' })).toBeTruthy()
})

test('Check asks the CLI; Disconnect asks first, then signs it out', async () => {
  let state: CliAccount['state'] = 'connected'
  const calls = fakeCli({
    'GET /api/me/cli-accounts': () => list(account('claude', 'Claude Code', { state })),
    'POST /api/me/cli-accounts/claude/check': () => {
      state = 'reconnect'
      return { body: account('claude', 'Claude Code', { state }) }
    },
    'DELETE /api/me/cli-accounts/claude': () => {
      state = 'disconnected'
      return { status: 204 }
    },
  })
  render(<CliAccountsTab />)
  fireEvent.click(await screen.findByRole('button', { name: 'Check Claude Code' }))
  expect(await screen.findByText('Needs you to sign in again')).toBeTruthy()
  expect(calls.find((c) => c.path.endsWith('/check'))?.body).toEqual({})

  fireEvent.click(screen.getByRole('button', { name: 'Disconnect Claude Code' }))
  expect(count(calls, 'DELETE', '/api/me/cli-accounts/claude')).toBe(0)
  expect(screen.getByText(/signs the CLI out on the server and deletes its sessions/)).toBeTruthy()
  fireEvent.click(screen.getByRole('button', { name: 'Yes, disconnect' }))
  expect(await screen.findByText('Not connected')).toBeTruthy()
  expect(count(calls, 'DELETE', '/api/me/cli-accounts/claude')).toBe(1)
})

test('usage limits: each window with how much is used, when it resets, warnings and the CLI words', () => {
  const resets = '2026-11-19T18:00:00Z'
  const asOf = '2026-11-19T15:02:11.120Z'
  render(
    <CliLimits
      account={account('claude', 'Claude Code', {
        state: 'connected',
        limits: [
          { window: 'five_hour', used_percent: 42, resets_at: resets, status: 'allowed' },
          { window: 'seven_day', used_percent: 91.4, status: 'allowed_warning' },
          { window: 'seven_day_opus', status: 'rejected', detail: 'Usage limit reached · resets 3pm' },
        ],
        limits_at: asOf,
      })}
    />,
  )
  const five = screen.getByRole('listitem', { name: '5-hour limit' })
  expect((within(five).getByLabelText('5-hour limit used') as HTMLMeterElement).value).toBe(42)
  expect(within(five).getByText('42% used')).toBeTruthy()
  expect(within(five).getByText(`Resets ${format().dateTime(resets)}`)).toBeTruthy()
  expect(within(five).queryByText(/limit reached|close to/i)).toBeNull()

  const week = screen.getByRole('listitem', { name: 'Weekly limit' })
  expect(week.getAttribute('data-status')).toBe('allowed_warning')
  expect(within(week).getByText('91% used')).toBeTruthy()
  expect(within(week).getByText('Close to the limit').className).toContain('tt-warning')

  const opus = screen.getByRole('listitem', { name: 'Weekly limit (Opus)' })
  expect(opus.getAttribute('data-status')).toBe('rejected')
  expect(within(opus).getByText('Limit reached').className).toContain('tt-error')
  expect(within(opus).getByText('Usage limit reached · resets 3pm')).toBeTruthy()
  expect(within(opus).queryByRole('meter')).toBeNull()

  expect(screen.getByText(`As of ${format().dateTime(asOf)}`)).toBeTruthy()
})

test("no limits yet, Grok's none, and Codex's windows named by their length", () => {
  render(<CliLimits account={account('codex', 'Codex', { state: 'connected' })} />)
  expect(screen.getByText('No usage reported yet.')).toBeTruthy()
  cleanup()
  render(<CliLimits account={account('grok', 'Grok Build', { state: 'connected' })} />)
  expect(screen.getByText("Grok Build doesn't report usage limits.")).toBeTruthy()
  expect(windowLabel({ window: 'primary', window_minutes: 300 })).toBe('5-hour limit')
  expect(windowLabel({ window: 'secondary', window_minutes: 10080 })).toBe('Weekly limit')
  expect(windowLabel({ window: 'secondary', window_minutes: 1440 })).toBe('1-day limit')
  expect(windowLabel({ window: 'secondary', window_minutes: 20160 })).toBe('2-week limit')
  expect(windowLabel({ window: 'secondary', window_minutes: 90 })).toBe('90-minute limit')
  expect(windowLabel({ window: 'overage_extra' })).toBe('Overage extra')
})

test('a sign-in the server keeps failing to report is read again less and less often', async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  const calls = fakeCli({
    'GET /api/me/cli-accounts': list(
      account('codex', 'Codex', {
        login: login('codex', { url: 'https://auth.openai.com/codex/device', code: 'AB-CD' }),
      }),
    ),
    'GET /api/me/cli-accounts/codex/login': { status: 500, body: { error: 'internal error' } },
  })
  render(<CliAccountsTab />)
  await screen.findByRole('link', { name: 'https://auth.openai.com/codex/device' })
  const reads = () => count(calls, 'GET', '/api/me/cli-accounts/codex/login')
  await poll() // 2 s: the first read fails
  await waitFor(() => expect(reads()).toBe(1))
  await poll() // 2 s more: the next waits 4 s, not yet
  expect(reads()).toBe(1)
  await poll()
  await waitFor(() => expect(reads()).toBe(2))
  expect((await screen.findByRole('alert')).textContent).toContain('internal error')
})

test('several accounts of one CLI: each its own card, a name, and another one added on the next number', async () => {
  const calls = fakeCli({
    'GET /api/me/cli-accounts': list(
      account('claude', 'Claude Code', { state: 'connected', label: 'Personal' }),
      account('claude', 'Claude Code', { id: 'claude-3', slot: 3, state: 'reconnect' }),
      account('codex', 'Codex'),
    ),
    'PATCH /api/me/cli-accounts/claude-3': {
      body: account('claude', 'Claude Code', { id: 'claude-3', slot: 3, label: 'Work' }),
    },
    'POST /api/me/cli-accounts/claude-2/login': {
      body: login('claude', { slot: '2', url: 'https://claude.com/cai/oauth/authorize?x=1', needs_code: true }),
    },
  })
  render(<CliAccountsTab />)
  const personal = await screen.findByRole('region', { name: 'Claude Code · Personal' })
  expect(within(personal).getByRole('button', { name: 'Disconnect Claude Code · Personal' })).toBeTruthy()
  const third = screen.getByRole('region', { name: 'Claude Code · account 3' })
  expect(within(third).getByRole('button', { name: 'Reconnect Claude Code · account 3' })).toBeTruthy()

  // Naming one changes that one.
  const name = within(third).getByRole('form', { name: 'Name of Claude Code · account 3' })
  fireEvent.change(within(name).getByLabelText('Name'), { target: { value: ' Work ' } })
  fireEvent.click(within(name).getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(count(calls, 'PATCH', '/api/me/cli-accounts/claude-3')).toBe(1))
  expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ label: 'Work' })

  // Another Claude Code account takes the first free number, with the warning about plans.
  expect(screen.getByText(/extra accounts to get around a plan's limits may break its terms/)).toBeTruthy()
  fireEvent.click(screen.getByRole('button', { name: 'Add another account Claude Code' }))
  await waitFor(() => expect(count(calls, 'POST', '/api/me/cli-accounts/claude-2/login')).toBe(1))
  // Codex's first account isn't connected: no second one is offered before it.
  expect(screen.queryByRole('button', { name: 'Add another account Codex' })).toBeNull()
})

test('a name typed right after the cards appear is kept: the form does not reset its draft in an effect that runs after the first commit', async () => {
  fakeCli({
    'GET /api/me/cli-accounts': list(account('claude', 'Claude Code', { state: 'connected', label: 'Personal' })),
  })
  // Outside act, as a real page runs: the cards are committed by the scheduler, and a person (or a
  // test) types as soon as the form is on screen, before the effects of that commit have run.
  const flag = globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }
  flag.IS_REACT_ACT_ENVIRONMENT = false
  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  try {
    root.render(<CliAccountsTab />)
    await new Promise<void>((resolve) => {
      const seen = new MutationObserver(() => {
        if (!host.querySelector('form[aria-label^="Name of"]')) return
        seen.disconnect()
        resolve()
      })
      seen.observe(host, { childList: true, subtree: true })
    })
    const input = host.querySelector('form[aria-label^="Name of"] input') as HTMLInputElement
    fireEvent.change(input, { target: { value: 'Work' } })
    await new Promise((resolve) => setTimeout(resolve, 50))
    expect(input.value).toBe('Work')
  } finally {
    root.unmount()
    host.remove()
    flag.IS_REACT_ACT_ENVIRONMENT = true
  }
})

test("a workspace's shared accounts: terms for API keys, the member's mode, and admins add one and set modes", async () => {
  const shared = {
    id: 4,
    cli: 'codex' as const,
    name: 'Codex',
    label: 'Company key',
    base_url: 'https://api.openai.com',
    model: '',
    api_key: 'set' as const,
    limits: [],
    limits_at: null,
    created_at: '2026-11-19T10:00:00Z',
  }
  const calls = fakeCli({
    'GET /api/me/cli-accounts': list(account('codex', 'Codex')),
    'GET /api/workspaces/1/cli-accounts': {
      body: { enabled: true, mode: 'own_else_shared', default_mode: 'own_else_shared', accounts: [shared] },
    },
    'GET /api/workspaces/1/cli-accounts/modes': {
      body: {
        default_mode: 'own_else_shared',
        members: [{ user_id: 2, name: 'Bea', email: 'bea@example.com', mode: null }],
      },
    },
    'POST /api/workspaces/1/cli-accounts': { status: 201, body: shared },
    'PATCH /api/workspaces/1/cli-accounts/modes': {
      body: { default_mode: 'own_else_shared', members: [] },
    },
    'POST /api/workspaces/1/cli-accounts/4/test': { body: { ok: true, message: 'the provider accepted the key' } },
  })
  render(<CliAccountsTab workspace={1} admin />)
  const panel = await screen.findByRole('region', { name: "This workspace's shared CLI accounts" })
  expect(within(panel).getByText(/never a subscription login/)).toBeTruthy()
  expect(within(panel).getByRole('link', { name: "OpenAI's Services Agreement" }).getAttribute('href')).toBe(
    'https://openai.com/policies/services-agreement/',
  )
  expect(within(panel).getByRole('link', { name: "Anthropic's Commercial Terms" })).toBeTruthy()
  expect(await within(panel).findByText(/use your own account when one is connected/)).toBeTruthy()

  const row = within(panel).getByRole('listitem', { name: 'Codex · Company key' })
  fireEvent.click(within(row).getByRole('button', { name: 'Test Codex · Company key' }))
  expect(await within(row).findByText('The provider accepted the key.')).toBeTruthy()

  fireEvent.click(within(panel).getByRole('button', { name: 'Add a shared account' }))
  const form = within(panel).getByRole('form', { name: 'New shared account' })
  fireEvent.change(within(form).getByLabelText('CLI'), { target: { value: 'claude' } })
  fireEvent.change(within(form).getByLabelText('Name'), { target: { value: 'Anthropic key' } })
  fireEvent.change(within(form).getByLabelText('API key'), { target: { value: 'fake-key-for-the-test' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Add' }))
  await waitFor(() => expect(count(calls, 'POST', '/api/workspaces/1/cli-accounts')).toBe(1))
  expect(calls.find((c) => c.method === 'POST' && c.path === '/api/workspaces/1/cli-accounts')?.body).toEqual({
    cli: 'claude',
    label: 'Anthropic key',
    model: '',
    api_key: 'fake-key-for-the-test',
  })

  fireEvent.change(await within(panel).findByLabelText('Accounts for Bea'), { target: { value: 'shared' } })
  await waitFor(() => expect(count(calls, 'PATCH', '/api/workspaces/1/cli-accounts/modes')).toBe(1))
  expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ members: { 2: 'shared' } })
})

test('members see the shared accounts without the buttons that change them', async () => {
  fakeCli({
    'GET /api/me/cli-accounts': list(account('codex', 'Codex')),
    'GET /api/workspaces/1/cli-accounts': {
      body: {
        enabled: false,
        reason: 'this server has no proxy for the CLIs',
        mode: 'shared',
        default_mode: 'own_else_shared',
        accounts: [],
      },
    },
  })
  render(<CliAccountsTab workspace={1} />)
  const panel = await screen.findByRole('region', { name: "This workspace's shared CLI accounts" })
  expect(await within(panel).findByText('this server has no proxy for the CLIs')).toBeTruthy()
  expect(within(panel).getByText(/use this workspace’s accounts only/)).toBeTruthy()
  expect(within(panel).queryByRole('button')).toBeNull()
})

test('automatic switching: off until turned on, with the warning and terms; the queue is ordered and added to', async () => {
  const own = (id: string, title: string, more: { label?: string; number?: number } = {}) => ({
    kind: 'own' as const,
    id,
    cli: 'claude' as const,
    title,
    limited: false,
    ...more,
  })
  const shared = {
    kind: 'shared' as const,
    id: '4',
    cli: 'codex' as const,
    title: 'Codex (Company key, Home)',
    workspace: 1,
    workspace_name: 'Home',
    limited: true,
    limited_until: '2026-11-19T18:00:00Z',
  }
  const calls = fakeFetch({
    'GET /api/me/cli-accounts': list(account('claude', 'Claude Code', { state: 'connected' })),
    'GET /api/me/cli-switching': {
      body: {
        enabled: false,
        queue: [own('claude', 'Claude Code'), shared],
        available: [
          own('claude', 'Claude Code'),
          own('claude-2', 'Claude Code (Work)'),
          // Named from its number, in the user's language, rather than by the server's title.
          own('claude-3', 'server title', { number: 3 }),
          shared,
        ],
      },
    },
    'PUT /api/me/cli-switching': { body: { enabled: true, queue: [], available: [] } },
  })
  render(<CliAccountsTab />)
  const panel = await screen.findByRole('region', { name: 'Automatic switching' })
  // The panel shows while its state loads: wait for the switch itself.
  const toggle = (await within(panel).findByRole('checkbox', {
    name: 'Switch automatically when an account stops',
  })) as HTMLInputElement
  expect(toggle.checked).toBe(false)
  expect(within(panel).getByText(/may break its terms/)).toBeTruthy()
  expect(within(panel).getByRole('link', { name: "Claude Code's terms" })).toBeTruthy()
  expect(within(panel).getByRole('link', { name: "OpenAI's Services Agreement" })).toBeTruthy()
  expect(within(panel).getByText(/At its limit until/)).toBeTruthy()
  expect(within(panel).getByRole('option', { name: 'Claude Code (account 3)' })).toBeTruthy()

  fireEvent.click(toggle)
  await waitFor(() => expect(count(calls, 'PUT', '/api/me/cli-switching')).toBe(1))
  expect(calls.filter((c) => c.method === 'PUT')[0].body).toEqual({ enabled: true })

  fireEvent.click(within(panel).getByRole('button', { name: 'Move Claude Code down' }))
  await waitFor(() => expect(count(calls, 'PUT', '/api/me/cli-switching')).toBe(2))
  expect(calls.filter((c) => c.method === 'PUT')[1].body).toEqual({ queue: [{ shared: 4 }, { account: 'claude' }] })

  fireEvent.change(within(panel).getByLabelText('Add to the queue'), { target: { value: 'own:claude-2' } })
  await waitFor(() => expect(count(calls, 'PUT', '/api/me/cli-switching')).toBe(3))
  expect(calls.filter((c) => c.method === 'PUT')[2].body).toEqual({
    queue: [{ account: 'claude' }, { shared: 4 }, { account: 'claude-2' }],
  })
})
