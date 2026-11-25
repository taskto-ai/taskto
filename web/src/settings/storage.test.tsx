// @vitest-environment jsdom

import { cleanup, render, screen, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { Me, MyStorage, SpaceUsage, StorageReport, Workspace } from '../api'
import { fakeFetch } from '../test/fakes'
import { SettingsPage } from './SettingsPage'

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [],
  can_create_workspace: false,
  is_operator: false,
}
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }
const member: Workspace = { id: 1, name: 'Home', role: 'member' }

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const MB = 1_000_000 // the sizes are shown in decimal units

function space(over: Partial<SpaceUsage>): SpaceUsage {
  return {
    id: 1,
    kind: 'shared',
    name: 'Team',
    knowledge_versioned: 0,
    knowledge_unversioned: 0,
    attachments: 0,
    total: 0,
    limit: 0,
    ...over,
  }
}

const report: StorageReport = {
  quota: { workspace: 100 * MB, user: 10 * MB },
  total: 14 * MB,
  spaces: [
    space({ id: 1, name: 'Team', knowledge_versioned: MB, knowledge_unversioned: 2 * MB, total: 3 * MB }),
    space({
      id: 2,
      kind: 'private',
      name: 'Private',
      owner: { id: 7, name: 'Bob', email: 'bob@example.com' },
      knowledge_unversioned: 8 * MB,
      attachments: 3 * MB,
      total: 11 * MB,
      limit: 10 * MB,
    }),
  ],
}

test('admins see every space, the biggest first, and each private space under its owner', async () => {
  const calls = fakeFetch({ 'GET /api/workspaces/1/usage': { body: report } })
  render(<SettingsPage me={me} workspace={admin} tab="storage" />)
  expect(await screen.findByText('14 MB of 100 MB')).toBeTruthy()
  expect(screen.getByText("Each member's private space may store up to 10 MB.")).toBeTruthy()
  const rows = screen.getAllByRole('listitem')
  expect(within(rows[0]).getByText("Bob's private space")).toBeTruthy()
  expect(within(rows[0]).getByText('bob@example.com')).toBeTruthy()
  expect(within(rows[0]).getByText('Documents 0 byte · Files 8 MB · Photos 3 MB')).toBeTruthy()
  expect(within(rows[0]).getByText('11 MB of 10 MB')).toBeTruthy()
  expect(within(rows[0]).getByRole('progressbar', { name: "Bob's private space" })).toBeTruthy()
  // A shared space has no limit of its own, so no bar.
  expect(within(rows[1]).getByText('Team')).toBeTruthy()
  expect(within(rows[1]).queryByRole('progressbar')).toBeNull()
  expect(calls.map((c) => c.path)).toEqual(['/api/workspaces/1/usage'])
})

test('without quotas the figures are shown with no bar', async () => {
  fakeFetch({ 'GET /api/workspaces/1/usage': { body: { ...report, quota: { workspace: 0, user: 0 } } } })
  render(<SettingsPage me={me} workspace={admin} tab="storage" />)
  expect(await screen.findByText('14 MB, no limit')).toBeTruthy()
  expect(screen.getByText("A member's private space has no limit.")).toBeTruthy()
})

test("a member sees their own space only, and never asks for the workspace's report", async () => {
  const mine: MyStorage = {
    quota: { workspace: 0, user: 10 * MB },
    space: space({
      id: 5,
      kind: 'private',
      name: 'Private',
      total: 4 * MB,
      knowledge_versioned: 4 * MB,
      limit: 10 * MB,
    }),
  }
  const calls = fakeFetch({ 'GET /api/workspaces/1/usage/me': { body: mine } })
  render(<SettingsPage me={me} workspace={member} tab="storage" />)
  expect(await screen.findByText('Your private space')).toBeTruthy()
  expect(screen.getByText('4 MB of 10 MB')).toBeTruthy()
  expect(calls.map((c) => c.path)).toEqual(['/api/workspaces/1/usage/me'])
})

test('a refused report is shown as an error', async () => {
  fakeFetch({ 'GET /api/workspaces/1/usage': { status: 403, body: { error: 'no', code: 'admin_only' } } })
  render(<SettingsPage me={me} workspace={admin} tab="storage" />)
  expect((await screen.findByRole('alert')).textContent).toBeTruthy()
})
