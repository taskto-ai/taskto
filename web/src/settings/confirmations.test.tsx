// @vitest-environment jsdom

// Every question Settings asks before an action that removes access or can't be undone: the answer
// decides. For each, Cancel (and the time for a request to go) sends nothing, and the action named
// on the button sends exactly its one request. The request comes after the dialog's answer, which is
// awaited, so each check waits for the page to settle before it counts.

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'
import type { CliAccount, Me, ProviderAccount, Space, SpaceMember, Workspace } from '../api'
import { type Call, fakeFetch } from '../test/fakes'
import { answer, chooseFromMenu } from '../test/ui'
import { resetToasts } from '../ui/Toast'
import { SettingsPage } from './SettingsPage'

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
  can_create_workspace: false,
  is_operator: false,
}
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }
const member: Workspace = { id: 1, name: 'Home', role: 'member' }

const people = [
  { id: 1, email: 'owner@example.com', name: 'Owner', role: 'admin', joined_at: '2026-11-01T00:00:00.000Z' },
  {
    id: 2,
    email: 'alice@example.com',
    name: 'Alice',
    role: 'member',
    joined_at: '2026-11-02T00:00:00.000Z',
    two_factor: true,
  },
  { id: 3, email: 'bob@example.com', name: 'Bob', role: 'member', joined_at: '2026-11-03T00:00:00.000Z' },
]

const membersTab = {
  'GET /api/workspaces/1/members': { body: { members: people } },
  'GET /api/workspaces/1/invites': {
    body: {
      invites: [
        {
          id: 4,
          email: 'pending@example.com',
          role: 'member',
          created_at: '2026-11-10T00:00:00.000Z',
          expires_at: '2026-11-17T00:00:00.000Z',
        },
      ],
    },
  },
  'GET /api/login/methods': { body: { password: true } },
}

const team: Space = { id: 4, workspace_id: 1, kind: 'shared', name: 'Team', archived_at: null, access: 'none' }
const teamMembers: SpaceMember[] = [{ id: 2, email: 'alice@example.com', name: 'Alice', role: 'editor', added_at: '' }]
const spacesTab = {
  'GET /api/workspaces/1/spaces?all=true': { body: { spaces: [team] } },
  'GET /api/workspaces/1/members': { body: { members: people } },
  'GET /api/workspaces/1/spaces/4/members': { body: { members: teamMembers } },
}

const own: ProviderAccount = {
  id: 9,
  scope: 'user',
  provider: 'u1-home',
  name: 'home',
  display_name: 'home',
  kind: 'openai-compatible',
  base_url: 'http://localhost:11434/v1',
  api_key: 'not set',
  models: [{ id: 'chat-model' }],
  created_at: '',
  updated_at: '',
}

const cli = (state: CliAccount['state']): CliAccount => ({
  id: 'claude',
  cli: 'claude',
  slot: 1,
  label: '',
  name: 'Claude Code',
  available: true,
  state,
  auth_method: 'claude.ai',
  limits: [],
  limits_at: null,
  login: null,
})
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
const cliTab = {
  'GET /api/me/cli-accounts': { body: { enabled: true, mode: 'local', accounts: [cli('connected')] } },
  'GET /api/me/cli-switching': { body: { enabled: false, queue: [], available: [] } },
  'GET /api/workspaces/1/cli-accounts': {
    body: { enabled: true, mode: 'own_else_shared', default_mode: 'own_else_shared', accounts: [shared] },
  },
  'GET /api/workspaces/1/cli-accounts/modes': { body: { default_mode: 'own_else_shared', members: [] } },
}

const session = (id: string, current: boolean, browser: string, os: string) => ({
  id,
  browser,
  os,
  created_at: '2026-11-20T10:00:00.000Z',
  last_seen_at: '2026-11-24T09:30:00.000Z',
  expires_at: '2026-12-20T10:00:00.000Z',
  current,
})

interface Case {
  name: string
  workspace: Workspace
  tab: string
  routes: Parameters<typeof fakeFetch>[0]
  // Brings up the question.
  ask: () => Promise<void>
  confirm: string
  method: string
  path: string
  // What the confirmed request sends: undefined for a request with no body.
  body: unknown
}

const CASES: Case[] = [
  {
    name: 'removing a member',
    workspace: admin,
    tab: 'members',
    routes: membersTab,
    ask: async () => {
      await screen.findByText('bob@example.com')
      chooseFromMenu('More actions for Bob', 'Remove')
    },
    confirm: 'Yes, remove',
    method: 'DELETE',
    path: '/api/workspaces/1/members/3',
    body: undefined,
  },
  {
    name: 'disabling a member',
    workspace: admin,
    tab: 'members',
    routes: membersTab,
    ask: async () => {
      await screen.findByText('alice@example.com')
      chooseFromMenu('More actions for Alice', 'Disable')
    },
    confirm: 'Yes, disable',
    method: 'POST',
    path: '/api/workspaces/1/members/2/disable',
    body: undefined,
  },
  {
    name: "clearing a member's two-factor login",
    workspace: admin,
    tab: 'members',
    routes: membersTab,
    ask: async () => {
      await screen.findByText('alice@example.com')
      chooseFromMenu('More actions for Alice', 'Clear two-factor')
    },
    confirm: 'Yes, clear it',
    method: 'DELETE',
    path: '/api/workspaces/1/members/2/two-factor',
    body: undefined,
  },
  {
    name: 'withdrawing an invite',
    workspace: admin,
    tab: 'members',
    routes: membersTab,
    ask: async () => {
      fireEvent.click(await screen.findByRole('button', { name: 'Withdraw the invite for pending@example.com' }))
    },
    confirm: 'Yes, withdraw',
    method: 'DELETE',
    path: '/api/workspaces/1/invites/4',
    body: undefined,
  },
  {
    name: 'archiving a shared space',
    workspace: admin,
    tab: 'spaces',
    routes: spacesTab,
    ask: async () => {
      if (!screen.queryByRole('dialog', { name: 'Team' })) {
        fireEvent.click(await screen.findByRole('button', { name: 'Manage Team' }))
      }
      const dialog = await screen.findByRole('dialog', { name: 'Team' })
      fireEvent.click(within(dialog).getByRole('button', { name: 'Archive' }))
    },
    confirm: 'Yes, archive',
    method: 'PATCH',
    path: '/api/workspaces/1/spaces/4',
    body: { archived: true },
  },
  {
    name: "removing a shared space's member",
    workspace: admin,
    tab: 'spaces',
    routes: spacesTab,
    ask: async () => {
      if (!screen.queryByRole('dialog', { name: 'Team' })) {
        fireEvent.click(await screen.findByRole('button', { name: 'Manage Team' }))
      }
      const dialog = await screen.findByRole('dialog', { name: 'Team' })
      fireEvent.click(await within(dialog).findByRole('button', { name: 'Remove Alice' }))
    },
    confirm: 'Yes, remove',
    method: 'DELETE',
    path: '/api/workspaces/1/spaces/4/members/2',
    body: undefined,
  },
  {
    name: 'removing a provider account',
    workspace: member,
    tab: 'providers',
    routes: {
      'GET /api/workspaces/1/providers': { body: { providers: [] } },
      'GET /api/me/providers': { body: { providers: [own] } },
    },
    ask: async () => {
      await screen.findByText('home')
      chooseFromMenu('More actions for home', 'Remove')
    },
    confirm: 'Yes, remove',
    method: 'DELETE',
    path: '/api/me/providers/9',
    body: undefined,
  },
  {
    name: 'removing a shared CLI account',
    workspace: admin,
    tab: 'cli-accounts',
    routes: cliTab,
    ask: async () => {
      await screen.findByText('Codex · Company key')
      chooseFromMenu('More actions for Codex · Company key', 'Remove')
    },
    confirm: 'Yes, remove',
    method: 'DELETE',
    path: '/api/workspaces/1/cli-accounts/4',
    body: undefined,
  },
  {
    name: 'disconnecting a CLI account',
    workspace: member,
    tab: 'cli-accounts',
    routes: cliTab,
    ask: async () => {
      fireEvent.click(await screen.findByRole('button', { name: 'Disconnect Claude Code' }))
    },
    confirm: 'Yes, disconnect',
    method: 'DELETE',
    path: '/api/me/cli-accounts/claude',
    body: undefined,
  },
  {
    name: 'signing out one session',
    workspace: member,
    tab: 'sessions',
    routes: {
      'GET /api/me/sessions': {
        body: {
          sessions: [session('session-3', true, 'Firefox', 'macOS'), session('session-2', false, 'Safari', 'iOS')],
        },
      },
    },
    ask: async () => {
      fireEvent.click(await screen.findByRole('button', { name: 'Sign out Safari on iOS' }))
    },
    confirm: 'Yes, sign out',
    method: 'DELETE',
    path: '/api/me/sessions/session-2',
    body: undefined,
  },
  {
    name: 'signing out every other session',
    workspace: member,
    tab: 'sessions',
    routes: {
      'GET /api/me/sessions': {
        body: {
          sessions: [session('session-3', true, 'Firefox', 'macOS'), session('session-2', false, 'Safari', 'iOS')],
        },
      },
    },
    ask: async () => {
      fireEvent.click(await screen.findByRole('button', { name: 'Sign out all other sessions' }))
    },
    confirm: 'Yes, sign out the others',
    method: 'DELETE',
    path: '/api/me/sessions',
    body: undefined,
  },
  {
    name: 'revoking an access token',
    workspace: member,
    tab: 'tokens',
    routes: {
      'GET /api/workspaces/1/tokens': {
        body: {
          tokens: [
            {
              id: 4,
              workspace_id: 1,
              name: 'ci script',
              created_at: '2026-11-01T00:00:00.000Z',
              expires_at: null,
              last_used_at: null,
            },
          ],
        },
      },
    },
    ask: async () => {
      fireEvent.click(await screen.findByRole('button', { name: 'Revoke ci script' }))
    },
    confirm: 'Yes, revoke',
    method: 'DELETE',
    path: '/api/workspaces/1/tokens/4',
    body: undefined,
  },
  {
    name: 'turning off your two-factor login',
    workspace: member,
    tab: 'password',
    routes: { 'GET /api/me/two-factor': { body: { enabled: true, recovery_codes_left: 3 } } },
    ask: async () => {
      await screen.findByText('Two-factor login is on.')
      fireEvent.change(screen.getByLabelText('Your password'), { target: { value: 'fake-password-123' } })
      fireEvent.change(screen.getByLabelText('Code from your app, or a recovery code'), {
        target: { value: '654321' },
      })
      fireEvent.click(screen.getByRole('button', { name: 'Turn off…' }))
    },
    confirm: 'Yes, turn off',
    method: 'POST',
    path: '/api/me/two-factor/disable',
    body: { password: 'fake-password-123', code: '654321' },
  },
]

const matching = (calls: Call[], c: Case) => calls.filter((x) => x.method === c.method && x.path === c.path)

// Long enough for a request that an answer would have started to be made and recorded.
const settle = () => act(() => new Promise((resolve) => setTimeout(resolve, 30)))

beforeEach(() => {
  window.history.replaceState(null, '', '/')
  localStorage.clear()
})
afterEach(() => {
  cleanup()
  act(() => resetToasts())
  vi.unstubAllGlobals()
})

describe.each(CASES)('$name', (c) => {
  test('Cancel sends nothing', async () => {
    const calls = fakeFetch(c.routes)
    render(<SettingsPage me={me} workspace={c.workspace} tab={c.tab} />)
    await c.ask()
    await screen.findByRole('alertdialog')
    await answer('Cancel')
    await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
    await settle()
    expect(matching(calls, c)).toEqual([])
  })

  test('Escape sends nothing', async () => {
    const calls = fakeFetch(c.routes)
    render(<SettingsPage me={me} workspace={c.workspace} tab={c.tab} />)
    await c.ask()
    const dialog = await screen.findByRole('alertdialog')
    fireEvent.keyDown(dialog, { key: 'Escape' })
    fireEvent(dialog, new Event('cancel'))
    await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
    await settle()
    expect(matching(calls, c)).toEqual([])
  })

  test('the named action sends exactly its request, once', async () => {
    const calls = fakeFetch(c.routes)
    render(<SettingsPage me={me} workspace={c.workspace} tab={c.tab} />)
    await c.ask()
    await answer(c.confirm)
    await waitFor(() => expect(matching(calls, c)).toHaveLength(1))
    await settle()
    expect(matching(calls, c).map((x) => x.body)).toEqual([c.body])
  })

  test('cancelled once, then confirmed: only the confirmed one is sent', async () => {
    const calls = fakeFetch(c.routes)
    render(<SettingsPage me={me} workspace={c.workspace} tab={c.tab} />)
    await c.ask()
    await answer('Cancel')
    await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
    await settle()
    await c.ask()
    await answer(c.confirm)
    await waitFor(() => expect(matching(calls, c)).toHaveLength(1))
    await settle()
    expect(matching(calls, c)).toHaveLength(1)
  })
})
