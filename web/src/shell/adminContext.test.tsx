// @vitest-environment jsdom
//
// The Apps section knows whether the person is an admin of the workspace it shows: the shell says it,
// from the role the server gave for that workspace, and nothing else does.

import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { Me } from '../api'
import { useIsAdmin } from '../apps/admin'
import { fakeFetch } from '../test/fakes'
import { Shell } from './Shell'

// The Apps page is a probe: it says what the shell told it.
vi.mock('../apps/AppsPage', () => ({
  AppsPage: ({ workspace }: { workspace: number }) => (
    <p>{`workspace ${workspace}: ${useIsAdmin() ? 'admin' : 'member'}`}</p>
  ),
}))

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [
    { id: 1, name: 'Home', role: 'admin' },
    { id: 4, name: 'Team', role: 'member' },
  ],
  can_create_workspace: false,
  is_operator: false,
}

test.each([
  [1, 'admin'],
  [4, 'member'],
])('in workspace %i the Apps section is told the person is a %s', async (workspace, role) => {
  fakeFetch({})
  window.history.replaceState(null, '', `/w/${workspace}/apps`)
  render(<Shell me={me} onLogout={() => undefined} />)
  expect(await screen.findByText(`workspace ${workspace}: ${role}`)).toBeTruthy()
})

function Probe() {
  return <p>{useIsAdmin() ? 'admin' : 'member'}</p>
}

test('without the shell nobody is an admin', () => {
  render(<Probe />)
  expect(screen.getByText('member')).toBeTruthy()
})
