// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
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
  vi.useRealTimers()
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
  request: { available: true },
  status: {
    version: 1,
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
    running: null,
    requests: { supported: true, last: null },
    available: null,
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

const requestRoute = 'POST /api/workspaces/1/updates/request'

test('an operator confirms Update now, and the request carries nothing', async () => {
  const calls = fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': { body: updates() },
    [requestRoute]: { status: 202, body: { requested_at: new Date().toISOString() } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Update now' }))
  expect(screen.getByText(/after a backup\. The server restarts/)).toBeTruthy()
  expect(calls.filter((c) => c.method === 'POST')).toEqual([])
  fireEvent.click(screen.getByRole('button', { name: 'Yes, update now' }))
  expect(await screen.findByText(/Update requested\. Waiting for the host to start it/)).toBeTruthy()
  const posts = calls.filter((c) => c.method === 'POST')
  expect(posts.length).toBe(1)
  expect(posts[0]?.path).toBe('/api/workspaces/1/updates/request')
  expect(posts[0]?.body).toEqual({})
  expect(screen.queryByRole('button', { name: 'Update now' })).toBeNull()
})

test('while an update runs the page says so, and the failed reads of its restart are not errors', async () => {
  let answer: { status?: number; body?: unknown } = {
    body: updates({}, { running: { since: ago(0.01), requested: true } }),
  }
  fakeFetch({ ...noBackups, 'GET /api/workspaces/1/updates': () => answer })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText(/Updating\. The server restarts during this/)).toBeTruthy()
  expect(screen.queryByRole('button', { name: 'Update now' })).toBeNull()
  answer = { status: 502, body: { error: 'bad gateway' } }
  await act(async () => {
    await new Promise((r) => setTimeout(r, 3300))
  })
  expect(await screen.findByText(/The server isn't answering/)).toBeTruthy()
  expect(screen.queryByRole('alert')).toBeNull()
  answer = {
    body: updates(
      { running_version: 'v0.4.0' },
      {
        recorded_version: 'v0.4.0',
        requests: { supported: true, last: { at: ago(0.2), finished: ago(0.01), result: 'updated' } },
      },
    ),
  }
}, 15000)

test('the first status read after Update now may take longer than the polling interval', async () => {
  let reads = 0
  let finish!: (reply: { body: UpdateStatus }) => void
  const slow = new Promise<{ body: UpdateStatus }>((resolve) => {
    finish = resolve
  })
  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': () => {
      reads++
      return reads === 1 ? { body: updates() } : slow
    },
    [requestRoute]: { status: 202, body: { requested_at: new Date().toISOString() } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Update now' }))
  vi.useFakeTimers()
  await act(async () => {
    fireEvent.click(screen.getByRole('button', { name: 'Yes, update now' }))
  })
  expect(reads).toBe(2)
  await act(() => vi.advanceTimersByTimeAsync(4000))
  expect(reads).toBe(2)
  await act(async () => {
    const at = new Date().toISOString()
    finish({ body: updates({}, { requests: { supported: true, last: { at, finished: at, result: 'updated' } } }) })
  })
  expect(screen.getByText(/The update you asked for: Updated/)).toBeTruthy()
})

test("Update now's refusals are said in words, with the way on for the host", async () => {
  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': {
      body: updates({ request: { available: false, reason: 'unsupported' } }),
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText(/The host can't take an update request yet/)).toBeTruthy()
  expect(screen.queryByRole('button', { name: 'Update now' })).toBeNull()
  cleanup()

  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': {
      body: updates({ request: { available: false, reason: 'off' } }, { channel: 'off' }),
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText(/Update now is off while automatic updates are off/)).toBeTruthy()
  cleanup()

  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': { body: updates() },
    [requestRoute]: { status: 409, body: { error: 'running', code: 'update_running' } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Update now' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, update now' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/An update is running now/)
})

test('an update a gate turned away says why, and points to the host', async () => {
  const at = ago(0.5)
  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': {
      body: updates({}, { requests: { supported: true, last: { at, finished: at, result: 'skipped', code: 'hold' } } }),
    },
    [requestRoute]: { status: 202, body: { requested_at: ago(1) } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Update now' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, update now' }))
  expect(await screen.findByText(/The update you asked for: Nothing done\./)).toBeTruthy()
  expect(screen.getAllByText(/Why: Updates are on hold\./).length).toBeGreaterThanOrEqual(1)
  expect(screen.getByText('taskto-update --to VERSION').tagName).toBe('CODE')
})

test("an admin who doesn't run the server never sees Update now", async () => {
  fakeFetch({})
  render(<SettingsPage me={notOperator} workspace={admin} tab="about" />)
  await screen.findByText(/Nothing this workspace uses now asks for a credit/)
  expect(screen.queryByRole('button', { name: 'Update now' })).toBeNull()
})

test('a request the host never took says so after a while, instead of waiting for ever', async () => {
  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': {
      body: updates({ request: { available: false, reason: 'pending', pending_since: ago(0.5) } }),
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText(/The host hasn't started the update/)).toBeTruthy()
  expect(screen.queryByText(/Update requested\. Waiting/)).toBeNull()
  cleanup()

  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': {
      body: updates({ request: { available: false, reason: 'pending', pending_since: ago(0.01) } }),
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText(/Update requested\. Waiting/)).toBeTruthy()
  expect(screen.queryByText(/The host hasn't started the update/)).toBeNull()
})

test('an update that waits shows its version, when it was published and its release notes, next to Update now', async () => {
  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': {
      body: updates(
        { notes_url: 'https://github.com/taskto-ai/taskto/releases/tag/v0.4.0' },
        {
          looked: ago(1),
          available: {
            version: 'v0.4.0',
            sha: 'a'.repeat(40),
            published: '2026-11-20T10:00:00Z',
            manual_upgrade: false,
          },
        },
      ),
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  const line = await screen.findByText(/^Update available:/)
  expect(line.textContent).toMatch(/Update available: v0\.4\.0 \(published .*2026/)
  expect(screen.getByText('v0.4.0').tagName).toBe('CODE')
  const notes = screen.getByRole('link', { name: 'Release notes' })
  expect(notes.getAttribute('href')).toBe('https://github.com/taskto-ai/taskto/releases/tag/v0.4.0')
  expect(notes.getAttribute('rel')).toContain('noopener')
  expect(screen.queryByText('Needs a person')).toBeNull()
  expect(screen.getByRole('button', { name: 'Update now' })).toBeTruthy()
  expect(screen.getByText('Last looked for updates')).toBeTruthy()
  expect(screen.queryByText(/Up to date: nothing newer/)).toBeNull()
})

test('a release that needs a person is badged, with the command that installs it', async () => {
  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': {
      body: updates(
        { notes_url: 'https://github.com/taskto-ai/taskto/releases/tag/v0.4.0' },
        {
          looked: ago(1),
          needs_person: { kind: 'manual-upgrade', code: 'manual-upgrade' },
          available: { version: 'v0.4.0', published: '2026-11-20T10:00:00Z', manual_upgrade: true },
        },
      ),
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  const badge = await screen.findByText('Needs a person')
  expect(badge.closest('.ui-badge')).toBeTruthy()
  expect(screen.getByText('taskto-update --to v0.4.0').tagName).toBe('CODE')
  expect(screen.getByRole('link', { name: 'Release notes' })).toBeTruthy()
})

test('on the main channel a waiting commit is shown short, with what changed since the running one', async () => {
  const sha = '0123456789abcdef0123456789abcdef01234567'
  fakeFetch({
    ...noBackups,
    'GET /api/workspaces/1/updates': {
      body: updates(
        {
          running_version: 'f'.repeat(40),
          notes_url: `https://github.com/taskto-ai/taskto/compare/${'f'.repeat(40)}...${sha}`,
        },
        {
          channel: 'main',
          recorded_version: 'f'.repeat(40),
          looked: ago(1),
          available: { version: sha, sha, manual_upgrade: false },
        },
      ),
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect((await screen.findByText(/^Update available:/)).textContent).toBe('Update available: 0123456789ab')
  expect(screen.getByText('0123456789ab').getAttribute('title')).toBe(sha)
  expect(screen.getByRole('link', { name: 'What changed' }).getAttribute('href')).toMatch(/\/compare\/f{40}\.\.\.0123/)
})

test('with nothing newer it says up to date, and an updater that never looked says nothing of it', async () => {
  fakeFetch({ ...noBackups, 'GET /api/workspaces/1/updates': { body: updates({}, { looked: ago(1) }) } })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  expect(await screen.findByText('Up to date: nothing newer on this channel.')).toBeTruthy()
  expect(screen.queryByText(/^Update available/)).toBeNull()
  cleanup()

  fakeFetch({ ...noBackups, 'GET /api/workspaces/1/updates': { body: updates() } })
  render(<SettingsPage me={me} workspace={admin} tab="about" />)
  await screen.findByText('Updates are working.')
  expect(screen.queryByText(/Up to date: nothing newer/)).toBeNull()
  expect(screen.queryByText('Last looked for updates')).toBeNull()
})
