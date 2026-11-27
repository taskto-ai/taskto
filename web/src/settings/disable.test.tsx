// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { Me, Member, Workspace } from '../api'
import { type Call, fakeFetch } from '../test/fakes'
import { answer, chooseFromMenu } from '../test/ui'
import { SettingsPage } from './SettingsPage'

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
  can_create_workspace: false,
  is_operator: false,
}
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }

const member = (over: Partial<Member>): Member => ({
  id: 2,
  email: 'alice@example.com',
  name: 'Alice',
  role: 'member',
  joined_at: '2026-11-02T00:00:00.000Z',
  disabled: false,
  ...over,
})

const sent = (calls: Call[], method: string, path: string) =>
  calls.filter((c) => c.method === method && c.path === path).length

beforeEach(() => {
  window.history.replaceState(null, '', '/')
  localStorage.clear()
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

test('an admin disables a member after a question, sees them marked, and enables them again', async () => {
  let people = [member({ id: 1, name: 'Owner', email: 'owner@example.com', role: 'admin' }), member({})]
  const calls = fakeFetch({
    'GET /api/workspaces/1/members': () => ({ body: { members: people } }),
    'GET /api/workspaces/1/invites': { body: { invites: [] } },
    'GET /api/login/methods': { body: { password: true } },
    'POST /api/workspaces/1/members/2/disable': () => {
      people = people.map((p) => (p.id === 2 ? { ...p, disabled: true } : p))
      return { status: 204 }
    },
    'POST /api/workspaces/1/members/2/enable': () => {
      people = people.map((p) => (p.id === 2 ? { ...p, disabled: false } : p))
      return { status: 204 }
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="members" />)
  const row = async () => (await screen.findByText('alice@example.com')).closest('tr') as HTMLElement

  await screen.findByText('owner@example.com')
  expect(screen.queryByRole('button', { name: 'More actions for Owner' })).toBeNull()

  await row()
  chooseFromMenu('More actions for Alice', 'Disable')
  expect(sent(calls, 'POST', '/api/workspaces/1/members/2/disable')).toBe(0)
  within(await screen.findByRole('alertdialog', { name: 'Disable Alice' })).getByText(/signed out everywhere/)
  await answer('Yes, disable')
  await within(await row()).findByText('Disabled')
  expect(sent(calls, 'POST', '/api/workspaces/1/members/2/disable')).toBe(1)

  fireEvent.click(screen.getByRole('button', { name: 'More actions for Alice' }))
  expect(screen.queryByRole('menuitem', { name: 'Disable' })).toBeNull()
  fireEvent.click(screen.getByRole('menuitem', { name: 'Enable' }))
  await waitFor(() =>
    expect(
      within(screen.getByText('alice@example.com').closest('tr') as HTMLElement).queryByText('Disabled'),
    ).toBeNull(),
  )
  expect(sent(calls, 'POST', '/api/workspaces/1/members/2/enable')).toBe(1)
})

test('a refusal is shown in the language, and the list stays as the server has it', async () => {
  fakeFetch({
    'GET /api/workspaces/1/members': { body: { members: [member({})] } },
    'GET /api/workspaces/1/invites': { body: { invites: [] } },
    'GET /api/login/methods': { body: { password: true } },
    'POST /api/workspaces/1/members/2/disable': {
      status: 403,
      body: { error: 'x', code: 'disable_elsewhere' },
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="members" />)
  await screen.findByText('alice@example.com')
  chooseFromMenu('More actions for Alice', 'Disable')
  await answer('Yes, disable')
  await screen.findByText(/only someone who administers all of their workspaces can disable or enable them/)
  expect(screen.queryByText('Disabled')).toBeNull()
})
