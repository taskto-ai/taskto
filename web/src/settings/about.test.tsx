// @vitest-environment jsdom

import { act, cleanup, render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { BackupStatus, Me, UpdateStatus, Workspace } from '../api'
import { setPreferences } from '../i18n'
import { type Call, fakeFetch } from '../test/fakes'
import { SettingsPage } from './SettingsPage'

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [],
  can_create_workspace: false,
  is_operator: true,
}
const notOperator: Me = { ...me, is_operator: false }
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

const noUpdater = {
  'GET /api/workspaces/1/updates': { body: { configured: false, running_version: 'dev', status: null } },
}
const noBackups = { 'GET /api/workspaces/1/backup': { body: { health: 'none' } } }

const asked = (calls: Call[]) => calls.filter((c) => c.path === '/api/workspaces/1/backup')

test('admins see when the last backup succeeded, the error since, and the schedule', async () => {
  fakeFetch({ ...noUpdater, 'GET /api/workspaces/1/backup': { body: failing() } })
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
  fakeFetch({ ...noUpdater, 'GET /api/workspaces/1/backup': { body: failing({ health: 'late', due_by: ago(1) }) } })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect((await screen.findByRole('alert')).textContent).toMatch(/A night was missed/)
  cleanup()

  fakeFetch({
    ...noUpdater,
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
  fakeFetch({ ...noUpdater, ...noBackups })
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

const updates = (
  over: Partial<UpdateStatus> = {},
  status: Partial<NonNullable<UpdateStatus['status']>> = {},
): UpdateStatus => ({
  configured: true,
  running_version: 'v0.3.0',
  status: {
    checked: ago(2),
    channel: 'release',
    recorded_version: 'v0.3.0',
    last_run: {
      at: ago(3),
      finished: ago(2.9),
      result: 'up-to-date',
      from: 'v0.3.0',
    },
    last_change: {
      at: ago(100),
      finished: ago(99),
      result: 'updated',
      from: 'v0.2.0',
      to: 'v0.3.0',
      backed_up: true,
    },
    needs_person: null,
    ...status,
  },
  ...over,
})

test('admins see the running version, the last run and change, and the version on record', async () => {
  fakeFetch({ ...noBackups, 'GET /api/workspaces/1/updates': { body: updates() } })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText('Updates are working.')).toBeTruthy()
  expect(screen.getAllByText('v0.3.0').length).toBeGreaterThanOrEqual(2)
  expect(screen.getByText(/Already up to date/)).toBeTruthy()
  expect(screen.getByText(/^Updated,/)).toBeTruthy()
  expect(screen.getByText(/v0\.2\.0 → v0\.3\.0/)).toBeTruthy()
  expect(screen.getByText(/A backup was made before it, on the host\./)).toBeTruthy()
  expect(screen.queryByRole('alert')).toBeNull()
})

test('a stop that needs a person is an alert with the command that clears it', async () => {
  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': {
      body: updates(
        {},
        {
          last_run: {
            finished: ago(1),
            result: 'stopped',
            from: 'v0.3.0',
            to: 'v0.4.0',
            code: 'stopped',
          },
          needs_person: { kind: 'blocked', code: 'stopped' },
        },
      ),
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect((await screen.findByRole('alert')).textContent).toMatch(/An update needs a person/)
  expect(screen.getByText('taskto-update --clear-block').tagName).toBe('CODE')
  expect(screen.getAllByText(/Why: The new server failed after it started, so the stack was stopped/).length).toBe(2)
  expect(screen.getByText(/The details are in the host's journal/)).toBeTruthy()
  expect(screen.getByText(/Failed, and the server was stopped/)).toBeTruthy()
})

test('a fresh failed run is not reported as working, even when nothing waits for a person', async () => {
  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': {
      body: updates({}, { last_run: { finished: ago(1), result: 'failed', from: 'v0.3.0', code: 'disk' } }),
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText(/The last update run failed/)).toBeTruthy()
  expect(screen.queryByText('Updates are working.')).toBeNull()
  expect(screen.getByText(/Why: There wasn't enough free disk/)).toBeTruthy()
})

test('a release that needs manual steps says so, and a version that is not on record is called out', async () => {
  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': {
      body: updates(
        { running_version: 'v0.3.1' },
        { needs_person: { kind: 'manual-upgrade', code: 'manual-upgrade' } },
      ),
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText(/A new release needs steps by hand/)).toBeTruthy()
  expect(screen.getByText('taskto-update --to VERSION').tagName).toBe('CODE')
  // The sentence wraps in a notice, never in a pill that would cut it off; the badge is short.
  const sentence = screen.getByText(/This is not the version the install records/)
  expect(sentence.closest('.ui-badge')).toBeNull()
  expect(sentence.closest('.ui-notice')?.getAttribute('data-tone')).toBe('warning')
  expect(screen.getByText('Not the recorded version').closest('.ui-badge')).toBeTruthy()
})

test('a stale status, a hold, and a server with no updater each say so, in the admin language', async () => {
  fakeFetch({ ...noBackups, 'GET /api/workspaces/1/updates': { body: updates({ stale: true }) } })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText(/The updater hasn't reported for days/)).toBeTruthy()
  cleanup()

  fakeFetch({ ...noBackups, 'GET /api/workspaces/1/updates': { body: updates({}, { hold: true }) } })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText(/Updates are on hold/)).toBeTruthy()
  cleanup()

  fakeFetch({ ...noBackups, ...noUpdater })
  await act(() => setPreferences({ user: 'pt-BR' }))
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText(/não está configurado para mostrar o estado do atualizador/)).toBeTruthy()
  await act(() => setPreferences({ user: null }))
  cleanup()

  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': { body: { configured: true, running_version: 'dev', status: null } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText(/The updater hasn't reported yet/)).toBeTruthy()
})

test('a release whose updater has written nothing is told its updater is too old to report', async () => {
  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': {
      body: { configured: true, running_version: 'v0.9.0', expects_status: true, status: null },
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText(/The updater on this host is too old to report/)).toBeTruthy()
  expect(screen.getByText('taskto-update --publish-status').tagName).toBe('CODE')
  expect(screen.queryByText(/The updater hasn't reported yet/)).toBeNull()
})

test('the updater only gives words from a list: an unknown code or result is shown as a general one', async () => {
  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': {
      body: updates(
        {},
        { last_run: { finished: ago(1), result: 'surprise', from: 'v0.3.0', code: '/var/backups/taskto/x.tar.gz' } },
      ),
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText(/Unknown, /)).toBeTruthy()
  expect(screen.getByText(/Why: The updater gave no more specific reason/)).toBeTruthy()
  expect(screen.queryByText(/backups\/taskto/)).toBeNull()
})

test("an admin who doesn't run the server doesn't see updates or ask for them", async () => {
  const calls = fakeFetch({})
  render(<SettingsPage me={notOperator} workspace={admin} tab="about" />)
  await screen.findByText(/Nothing this workspace uses now asks for a credit/)
  expect(screen.queryByRole('heading', { name: 'Updates' })).toBeNull()
  expect(calls.filter((c) => c.path === '/api/workspaces/1/updates')).toEqual([])
})

test("members don't see updates, and don't ask for them", async () => {
  const calls = fakeFetch({})
  render(<SettingsPage me={me} workspace={member} tab="about" />)
  await screen.findByText(/Nothing this workspace uses now asks for a credit/)
  expect(screen.queryByRole('heading', { name: 'Updates' })).toBeNull()
  expect(calls.filter((c) => c.path === '/api/workspaces/1/updates')).toEqual([])
})

test("an admin of a workspace they made, who doesn't run the server, doesn't see backups or ask for them", async () => {
  const calls = fakeFetch({})
  render(<SettingsPage me={notOperator} workspace={admin} tab="about" />)
  await screen.findByText(/Nothing this workspace uses now asks for a credit/)
  expect(screen.queryByRole('heading', { name: 'Backups' })).toBeNull()
  expect(asked(calls)).toEqual([])
})
