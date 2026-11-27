// @vitest-environment jsdom

import { cleanup, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'
import type { Me, ProviderAccount, Workspace } from '../api'
import { fakeFetch } from '../test/fakes'
import { SettingsPage } from './SettingsPage'

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
  can_create_workspace: false,
  is_operator: false,
}
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }
const member: Workspace = { id: 1, name: 'Home', role: 'member' }

const gateway: ProviderAccount = {
  id: 4,
  scope: 'workspace',
  provider: 'ws1-gateway',
  name: 'gateway',
  display_name: 'Team gateway',
  kind: 'openai-compatible',
  base_url: 'https://llm.example.com/v1',
  api_key: 'set',
  models: [{ id: 'chat-model' }],
  created_at: '',
  updated_at: '',
}

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

beforeEach(() => {
  window.history.replaceState(null, '', '/')
  localStorage.clear()
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe("the workspace's provider accounts", () => {
  const routes = (providers: ProviderAccount[]) => ({
    'GET /api/workspaces/1/providers': { body: { providers } },
    'GET /api/me/providers': { body: { providers: [] } },
  })
  const workspaceAccounts = () => screen.findByRole('region', { name: "This workspace's accounts" })

  test('empty: a member is told there are none, with no way to add one', async () => {
    fakeFetch(routes([]))
    render(<SettingsPage me={me} workspace={member} tab="providers" />)
    const region = await workspaceAccounts()
    await within(region).findByText('No accounts yet.')
    expect(within(region).queryByRole('button')).toBeNull()
    expect(within(region).queryByText('Add one to use its models in conversations.')).toBeNull()
  })

  test('empty: an admin is offered to add one', async () => {
    fakeFetch(routes([]))
    render(<SettingsPage me={me} workspace={admin} tab="providers" />)
    const region = await workspaceAccounts()
    expect(await within(region).findByRole('button', { name: 'Add an account' })).toBeTruthy()
  })

  test('with an account: a member reads it, with no test, edit or remove', async () => {
    fakeFetch(routes([gateway]))
    render(<SettingsPage me={me} workspace={member} tab="providers" />)
    const region = await workspaceAccounts()
    await within(region).findByText('Team gateway')
    expect(within(region).getByText('Key set')).toBeTruthy()
    expect(within(region).queryByRole('button')).toBeNull()
  })

  test('with an account: an admin has test, the more menu and add', async () => {
    fakeFetch(routes([gateway]))
    render(<SettingsPage me={me} workspace={admin} tab="providers" />)
    const region = await workspaceAccounts()
    await within(region).findByText('Team gateway')
    expect(within(region).getByRole('button', { name: 'Test Team gateway' })).toBeTruthy()
    expect(within(region).getByRole('button', { name: 'More actions for Team gateway' })).toBeTruthy()
    expect(within(region).getByRole('button', { name: 'Add an account' })).toBeTruthy()
  })
})

describe("the workspace's shared CLI accounts", () => {
  const routes = () =>
    fakeFetch({
      'GET /api/me/cli-accounts': { body: { enabled: true, mode: 'local', accounts: [] } },
      'GET /api/me/cli-switching': { body: { enabled: false, queue: [], available: [] } },
      'GET /api/workspaces/1/cli-accounts': {
        body: { enabled: true, mode: 'own_else_shared', default_mode: 'own_else_shared', accounts: [shared] },
      },
      'GET /api/workspaces/1/cli-accounts/modes': {
        body: {
          default_mode: 'own_else_shared',
          members: [{ user_id: 2, name: 'Bea', email: 'bea@example.com', mode: null }],
        },
      },
    })
  const panel = () => screen.findByRole('region', { name: "This workspace's shared CLI accounts" })

  test('a member reads the accounts, without add, test, the more menu or the modes', async () => {
    const calls = routes()
    render(<SettingsPage me={me} workspace={member} tab="cli-accounts" />)
    const region = await panel()
    await within(region).findByText('Codex · Company key')
    expect(within(region).queryByRole('button')).toBeNull()
    expect(within(region).queryByRole('heading', { name: "Which accounts members' turns use" })).toBeNull()
    expect(within(region).queryByLabelText('Accounts for Bea')).toBeNull()
    expect(calls.filter((c) => c.path.endsWith('/modes'))).toEqual([])
  })

  test('an admin has add, test, the more menu and each member’s accounts', async () => {
    routes()
    render(<SettingsPage me={me} workspace={admin} tab="cli-accounts" />)
    const region = await panel()
    await within(region).findByText('Codex · Company key')
    expect(within(region).getByRole('button', { name: 'Add a shared account' })).toBeTruthy()
    expect(within(region).getByRole('button', { name: 'Test Codex · Company key' })).toBeTruthy()
    expect(within(region).getByRole('button', { name: 'More actions for Codex · Company key' })).toBeTruthy()
    expect(await within(region).findByLabelText('Accounts for Bea')).toBeTruthy()
  })
})
