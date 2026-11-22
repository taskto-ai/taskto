// @vitest-environment jsdom

import { act, cleanup, render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { BackupStatus, Me, Workspace } from '../api'
import { setPreferences } from '../i18n'
import { type Call, fakeFetch } from '../test/fakes'
import { SettingsPage } from './SettingsPage'

const me: Me = { user: { id: 1, email: 'owner@example.com', name: 'Owner' }, workspaces: [] }
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }
const member: Workspace = { id: 1, name: 'Home', role: 'member' }

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const hour = 3600_000
const ago = (h: number) => new Date(Date.now() - h * hour).toISOString()

function failing(over: Partial<BackupStatus> = {}): BackupStatus {
  return {
    health: 'failing',
    at: '03:15',
    retries: 6,
    retry_every_seconds: 3600,
    last_success: ago(30),
    last_snapshot: '0123abcd4567ef89',
    last_attempt: ago(1),
    last_error: "the repository didn't answer, so nothing was backed up: connection refused",
    last_error_at: ago(1),
    next_attempt: ago(-1),
    due_by: ago(-2),
    ...over,
  }
}

const asked = (calls: Call[]) => calls.filter((c) => c.path === '/api/workspaces/1/backup')

test('admins see when the last backup succeeded, the error since, and the schedule', async () => {
  fakeFetch({ 'GET /api/workspaces/1/backup': { body: failing() } })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText('The last backup failed. The schedule tries again at the time below.')).toBeTruthy()
  expect(screen.getByText(/connection refused/).tagName).toBe('CODE')
  expect(screen.getByText('0123abcd')).toBeTruthy()
  expect(
    screen.getByText(/Every night at 03:15 UTC\. A failed night is tried again up to 6 times, every 60 minutes\./),
  ).toBeTruthy()
  expect(screen.queryByText(/has been running/)).toBeNull()
})

test('a missed night is an alert; a later success hides an old error; a running backup says so', async () => {
  fakeFetch({ 'GET /api/workspaces/1/backup': { body: failing({ health: 'late', due_by: ago(1) }) } })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect((await screen.findByRole('alert')).textContent).toMatch(/A night was missed/)
  cleanup()

  fakeFetch({
    'GET /api/workspaces/1/backup': {
      body: failing({ health: 'ok', last_success: ago(0.5), last_attempt: ago(0.1), last_error_at: ago(2) }),
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  await screen.findByText('Backups are on time.')
  expect(screen.queryByText(/connection refused/)).toBeNull()
  expect(screen.getByText(/A backup has been running since/)).toBeTruthy()
})

test('a server without scheduled backups says so, in the admin language', async () => {
  fakeFetch({ 'GET /api/workspaces/1/backup': { body: { health: 'none' } } })
  await act(() => setPreferences({ user: 'pt-BR' }))
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText(/Nenhum backup agendado rodou neste servidor/)).toBeTruthy()
  expect(screen.queryByText('Último backup bem-sucedido')).toBeNull()
  await act(() => setPreferences({ user: null }))
})

test("members don't see backups, and don't ask for them", async () => {
  const calls = fakeFetch({})
  render(<SettingsPage me={me} workspace={member} tab="about" />)
  await screen.findByText(/Nothing this workspace uses now asks for a credit/)
  expect(screen.queryByRole('heading', { name: 'Backups' })).toBeNull()
  expect(asked(calls)).toEqual([])
})
