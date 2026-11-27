// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { Me, Session, Workspace } from '../api'
import { type Call, fakeFetch } from '../test/fakes'
import { SettingsPage } from './SettingsPage'

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [{ id: 1, name: 'Home', role: 'member' }],
  can_create_workspace: false,
  is_operator: false,
}
const member: Workspace = { id: 1, name: 'Home', role: 'member' }

const session = (over: Partial<Session>): Session => ({
  id: 'session-1',
  browser: 'Firefox',
  os: 'macOS',
  created_at: '2026-11-20T10:00:00.000Z',
  last_seen_at: '2026-11-24T09:30:00.000Z',
  expires_at: '2026-12-20T10:00:00.000Z',
  current: false,
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

test('sessions list the device, the dates and which one is this device', async () => {
  fakeFetch({
    'GET /api/me/sessions': {
      body: {
        sessions: [
          session({ id: 'session-3', current: true }),
          session({ id: 'session-2', browser: 'Safari', os: 'iOS' }),
          session({ id: 'session-1', browser: '', os: '' }),
        ],
      },
    },
  })
  render(<SettingsPage me={me} workspace={member} tab="sessions" />)
  const list = await screen.findByRole('list', { name: 'Your sessions' })
  const rows = within(list).getAllByRole('listitem')
  expect(rows).toHaveLength(3)
  expect(rows[0].textContent).toContain('Firefox on macOS')
  expect(rows[0].textContent).toContain('This device')
  expect(rows[0].textContent).toContain('Signed in')
  expect(rows[0].textContent).toContain('last used')
  expect(within(rows[0]).queryByRole('button')).toBeNull()
  expect(rows[1].textContent).toContain('Safari on iOS')
  expect(within(rows[1]).getByRole('button', { name: 'Sign out Safari on iOS' })).toBeTruthy()
  expect(rows[2].textContent).toContain('Unknown device')
})

test('signing out one session asks first, sends the request and shows the list as it is then', async () => {
  let sessions = [
    session({ id: 'session-3', current: true }),
    session({ id: 'session-2', browser: 'Safari', os: 'iOS' }),
  ]
  const calls = fakeFetch({
    'GET /api/me/sessions': () => ({ body: { sessions } }),
    'DELETE /api/me/sessions/session-2': () => {
      sessions = sessions.filter((s) => s.id !== 'session-2')
      return { status: 204 }
    },
  })
  render(<SettingsPage me={me} workspace={member} tab="sessions" />)
  await screen.findByText('Safari on iOS')
  fireEvent.click(screen.getByRole('button', { name: 'Sign out Safari on iOS' }))
  expect(sent(calls, 'DELETE', '/api/me/sessions/session-2')).toBe(0)
  fireEvent.click(screen.getByRole('button', { name: 'Yes, sign out' }))
  await waitFor(() => expect(screen.queryByText('Safari on iOS')).toBeNull())
  expect(sent(calls, 'DELETE', '/api/me/sessions/session-2')).toBe(1)
  await screen.findByText('No other sessions.')
})

test('signing out all the others is offered only with others, and asks first', async () => {
  let sessions = [
    session({ id: 'session-3', current: true }),
    session({ id: 'session-2' }),
    session({ id: 'session-1', os: 'Linux' }),
  ]
  const calls = fakeFetch({
    'GET /api/me/sessions': () => ({ body: { sessions } }),
    'DELETE /api/me/sessions': () => {
      sessions = sessions.filter((s) => s.current)
      return { body: { revoked: 2 } }
    },
  })
  render(<SettingsPage me={me} workspace={member} tab="sessions" />)
  await screen.findByRole('list', { name: 'Your sessions' })
  fireEvent.click(screen.getByRole('button', { name: 'Sign out all other sessions' }))
  expect(sent(calls, 'DELETE', '/api/me/sessions')).toBe(0)
  fireEvent.click(screen.getByRole('button', { name: 'Yes, sign out the others' }))
  await screen.findByText('No other sessions.')
  expect(sent(calls, 'DELETE', '/api/me/sessions')).toBe(1)
  expect(within(screen.getByRole('list', { name: 'Your sessions' })).getAllByRole('listitem')).toHaveLength(1)
})

test('a failed sign-out shows the translated error', async () => {
  fakeFetch({
    'GET /api/me/sessions': {
      body: { sessions: [session({ id: 'session-3', current: true }), session({ id: 'session-2' })] },
    },
    'DELETE /api/me/sessions/session-2': {
      status: 404,
      body: { error: 'session not found', code: 'session_not_found' },
    },
  })
  render(<SettingsPage me={me} workspace={member} tab="sessions" />)
  await screen.findByRole('list', { name: 'Your sessions' })
  fireEvent.click(screen.getByRole('button', { name: 'Sign out Firefox on macOS' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, sign out' }))
  await screen.findByText('Session not found: it may have been signed out already.')
})
