// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { type DeletionStatus, type Me, type Member, type PendingDeletion, SESSION_LOST, type Workspace } from '../api'
import { clearDeletion, peekDeletion } from '../auth/deleted'
import { type Call, fakeFetch } from '../test/fakes'
import { answer } from '../test/ui'
import { claimDrafts, readState, writeState } from '../uiState'
import { SettingsPage } from './SettingsPage'

const me = (operator: boolean): Me => ({
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
  can_create_workspace: false,
  is_operator: operator,
})
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }
const plain: Workspace = { id: 1, name: 'Home', role: 'member' }

const status = (over: Partial<DeletionStatus> = {}): DeletionStatus => ({
  grace_days: 14,
  method: 'password',
  two_factor: false,
  fresh: false,
  blockers: [],
  ...over,
})

const sent = (calls: Call[], method: string, path: string) =>
  calls.filter((c) => c.method === method && c.path === path)

beforeEach(() => {
  window.history.replaceState(null, '', '/')
  localStorage.clear()
  sessionStorage.clear()
  clearDeletion() // a date a test left in memory
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

test('deleting your account offers the export first, asks for the password, and ends in the sign-in screen with the date', async () => {
  const calls = fakeFetch({
    'GET /api/me/deletion': { body: status() },
    'POST /api/workspaces/1/export': { body: { download: '/api/workspaces/1/export/x', expires_in: 60 } },
    'POST /api/me/deletion': { status: 202, body: { purge_at: '2026-12-08T10:00:00.000Z' } },
  })
  const lost = vi.fn()
  window.addEventListener(SESSION_LOST, lost)
  render(<SettingsPage me={me(false)} workspace={plain} tab="data" />)
  await screen.findByRole('heading', { name: 'Delete your account' })
  // Looking at the page asks the server nothing.
  expect(calls).toEqual([])
  fireEvent.click(screen.getByRole('button', { name: 'Delete my account…' }))
  // What it does, in plain words, before the form.
  await screen.findByText(/kept for 14 days/)
  screen.getByText(/shown as “Deleted user”/)
  expect(screen.getByRole('button', { name: 'Download my data first' })).toBeTruthy()

  const form = screen.getByRole('form', { name: 'Delete your account' })
  // Nothing is sent until the password is given.
  expect(sent(calls, 'POST', '/api/me/deletion')).toHaveLength(0)
  expect(within(form).queryByLabelText('Two-factor code or recovery code')).toBeNull()
  fireEvent.change(within(form).getByLabelText('Your password'), { target: { value: 'fake-test-password' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Delete my account' }))

  await waitFor(() => expect(sent(calls, 'POST', '/api/me/deletion')).toHaveLength(1))
  expect(sent(calls, 'POST', '/api/me/deletion')[0].body).toEqual({ password: 'fake-test-password' })
  // Bound to the account whose form it is (the owner, 1), so a stale tab can't delete another's.
  expect(Object.values(sent(calls, 'POST', '/api/me/deletion')[0].headers)).toContain('1')
  await waitFor(() => expect(lost).toHaveBeenCalled())
  // The end of the session is the owner's (1): a window signed in as another account ignores it.
  expect((lost.mock.calls[0][0] as CustomEvent).detail).toEqual({ user: 1 })
  // The sign-in screen is told when the account is erased: the date waits until a screen has drawn it.
  expect(peekDeletion()).toBe('2026-12-08T10:00:00.000Z')
  expect(peekDeletion()).toBe('2026-12-08T10:00:00.000Z')
  window.removeEventListener(SESSION_LOST, lost)
})

test('the answer arriving after another account signed in here, in another tab, erases only the deleted account’s drafts', async () => {
  claimDrafts(1)
  writeState('taskto:message:1:c1', 'the leaver’s unsent text')
  fakeFetch({
    'GET /api/me/deletion': { body: status() },
    'POST /api/me/deletion': () => {
      // Before the answer, account 2 signs in through another tab and writes a draft.
      writeState('taskto:drafts-of', '2')
      writeState('taskto:message:2:c1', 'the other account’s unsent text')
      return { status: 202, body: { purge_at: '2026-12-08T10:00:00.000Z' } }
    },
  })
  const lost = vi.fn()
  window.addEventListener(SESSION_LOST, lost)
  render(<SettingsPage me={me(false)} workspace={plain} tab="data" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Delete my account…' }))
  const form = await screen.findByRole('form', { name: 'Delete your account' })
  fireEvent.change(within(form).getByLabelText('Your password'), { target: { value: 'fake-test-password' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Delete my account' }))
  await waitFor(() => expect(lost).toHaveBeenCalled())
  expect(readState('taskto:message:1:c1')).toBeNull()
  expect(readState('taskto:message:2:c1')).toBe('the other account’s unsent text')
  expect(readState('taskto:drafts-of')).toBe('2')
  window.removeEventListener(SESSION_LOST, lost)
})

test('an account with two-factor login sends its code too, and a wrong password is shown and nothing ends', async () => {
  const calls = fakeFetch({
    'GET /api/me/deletion': { body: status({ two_factor: true }) },
    'POST /api/me/deletion': { status: 403, body: { error: 'x', code: 'wrong_password' } },
  })
  const lost = vi.fn()
  window.addEventListener(SESSION_LOST, lost)
  render(<SettingsPage me={me(false)} workspace={plain} tab="data" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Delete my account…' }))
  const form = await screen.findByRole('form', { name: 'Delete your account' })
  fireEvent.change(within(form).getByLabelText('Your password'), { target: { value: 'wrong-password-here' } })
  fireEvent.change(within(form).getByLabelText('Two-factor code or recovery code'), { target: { value: '123456' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Delete my account' }))
  await screen.findByRole('alert')
  expect(sent(calls, 'POST', '/api/me/deletion')[0].body).toEqual({ password: 'wrong-password-here', code: '123456' })
  expect(lost).not.toHaveBeenCalled()
  expect(peekDeletion()).toBeNull()
  window.removeEventListener(SESSION_LOST, lost)
})

test('a deletion refused because its session ended already, in another tab, ends the session of this account only and remembers no date', async () => {
  fakeFetch({
    'GET /api/me/deletion': { body: status() },
    'POST /api/me/deletion': { status: 401, body: { error: 'log in first', code: 'not_logged_in' } },
  })
  const lost = vi.fn()
  window.addEventListener(SESSION_LOST, lost)
  render(<SettingsPage me={me(false)} workspace={plain} tab="data" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Delete my account…' }))
  const form = await screen.findByRole('form', { name: 'Delete your account' })
  fireEvent.change(within(form).getByLabelText('Your password'), { target: { value: 'fake-test-password' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Delete my account' }))
  await waitFor(() => expect(lost).toHaveBeenCalled())
  // Told for the owner (1), whose form this is: a window signed in as another account ignores it.
  expect((lost.mock.calls[0][0] as CustomEvent).detail).toEqual({ user: 1 })
  expect(peekDeletion()).toBeNull()
  window.removeEventListener(SESSION_LOST, lost)
})

test('the only admin of a workspace is told which, and can not start', async () => {
  fakeFetch({
    'GET /api/me/deletion': {
      body: status({ blockers: [{ workspace_id: 1, workspace: 'Home', operator: true }] }),
    },
  })
  render(<SettingsPage me={me(true)} workspace={admin} tab="data" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Delete my account…' }))
  await screen.findByText(/You are the only admin of Home, which runs this server/)
  const form = screen.getByRole('form', { name: 'Delete your account' })
  expect((within(form).getByRole('button', { name: 'Delete my account' }) as HTMLButtonElement).disabled).toBe(true)
})

test('an account with no password needs a recent sign-in, and then sends no password', async () => {
  let fresh = false
  const calls = fakeFetch({
    'GET /api/me/deletion': () => ({ body: status({ method: 'fresh_login', fresh }) }),
    'POST /api/me/deletion': { status: 202, body: { purge_at: '2026-12-08T10:00:00.000Z' } },
  })
  const view = render(<SettingsPage me={me(false)} workspace={plain} tab="data" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Delete my account…' }))
  await screen.findByText(/Sign in again with your provider/)
  expect(screen.queryByRole('form', { name: 'Delete your account' })).toBeNull()
  view.unmount()

  fresh = true
  render(<SettingsPage me={me(false)} workspace={plain} tab="data" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Delete my account…' }))
  const form = await screen.findByRole('form', { name: 'Delete your account' })
  expect(within(form).queryByLabelText('Your password')).toBeNull()
  fireEvent.click(within(form).getByRole('button', { name: 'Delete my account' }))
  await waitFor(() => expect(sent(calls, 'POST', '/api/me/deletion')).toHaveLength(1))
  expect(sent(calls, 'POST', '/api/me/deletion')[0].body).toEqual({})
})

const member = (over: Partial<Member>): Member => ({
  id: 2,
  email: 'alice@example.com',
  name: 'Alice',
  role: 'member',
  joined_at: '2026-11-02T00:00:00.000Z',
  disabled: false,
  deletion_at: null,
  ...over,
})

test('an admin has no action that deletes a member’s account: only the person deletes theirs', async () => {
  const people = [
    member({ id: 1, name: 'Owner', email: 'owner@example.com', role: 'admin' }),
    member({}),
    member({ id: 3, name: 'Bob', email: 'bob@example.com', disabled: true, deletion_at: '2026-12-08T10:00:00.000Z' }),
  ]
  const calls = fakeFetch({
    'GET /api/workspaces/1/members': { body: { members: people } },
    'GET /api/workspaces/1/invites': { body: { invites: [] } },
    'GET /api/login/methods': { body: { password: true } },
  })
  render(<SettingsPage me={me(false)} workspace={admin} tab="members" />)
  await screen.findByText('alice@example.com')

  // Not in the menu of an ordinary member, of one who is waiting to be deleted, nor anywhere else:
  // an admin can remove a member from the workspace, and that is all they can do about the account.
  fireEvent.click(screen.getByRole('button', { name: 'More actions for Alice' }))
  expect(screen.getByRole('menuitem', { name: 'Remove' })).toBeTruthy()
  expect(screen.queryByRole('menuitem', { name: /Delete account/ })).toBeNull()
  fireEvent.keyDown(screen.getByRole('menuitem', { name: 'Remove' }), { key: 'Escape' })
  // The row of an account that waits to be deleted shows when, with nothing to act on it but removing.
  const row = screen.getByText('bob@example.com').closest('tr') as HTMLElement
  await within(row).findByText(/Deleting on/)
  fireEvent.click(screen.getByRole('button', { name: 'More actions for Bob' }))
  expect(screen.queryByRole('menuitem', { name: /Delete account/ })).toBeNull()
  expect(screen.queryByRole('menuitem', { name: 'Enable' })).toBeNull()
  expect(screen.queryByRole('menuitem', { name: 'Disable' })).toBeNull()
  // A plain admin sees no list of waiting accounts: that is the operator's.
  expect(screen.queryByRole('heading', { name: 'Accounts waiting to be deleted' })).toBeNull()
  expect(sent(calls, 'GET', '/api/workspaces/1/account-deletions')).toHaveLength(0)
  expect(calls.filter((c) => c.path.endsWith('/deletion'))).toHaveLength(0)
})

test('an operator sees every account waiting to be deleted and cancels one', async () => {
  let pending: PendingDeletion[] = [
    {
      user: { id: 2, email: 'alice@example.com', name: 'Alice' },
      requested_at: '2026-11-24T10:00:00.000Z',
      purge_at: '2026-12-08T10:00:00.000Z',
      purging: false,
    },
    {
      user: { id: 3, email: 'bob@example.com', name: 'Bob' },
      requested_at: '2026-11-01T10:00:00.000Z',
      purge_at: '2026-11-15T10:00:00.000Z',
      purging: true,
    },
  ]
  const calls = fakeFetch({
    'GET /api/workspaces/1/members': {
      body: { members: [member({ id: 1, name: 'Owner', email: 'owner@example.com', role: 'admin' })] },
    },
    'GET /api/workspaces/1/invites': { body: { invites: [] } },
    'GET /api/login/methods': { body: { password: true } },
    'GET /api/workspaces/1/account-deletions': () => ({ body: { deletions: pending } }),
    'DELETE /api/workspaces/1/account-deletions/2': () => {
      pending = pending.filter((p) => p.user.id !== 2)
      return { status: 204 }
    },
  })
  render(<SettingsPage me={me(true)} workspace={admin} tab="members" />)
  const list = await screen.findByRole('list', { name: 'Accounts waiting to be deleted' })
  const alice = within(list).getByText('Alice').closest('li') as HTMLElement
  const bob = within(list).getByText('Bob').closest('li') as HTMLElement
  expect(within(alice).getByText(/Purged on/)).toBeTruthy()
  // Once the purge has begun it can no longer be cancelled, and the list says so.
  expect(within(bob).getByText(/Being purged now/)).toBeTruthy()
  expect(within(bob).queryByRole('button')).toBeNull()

  fireEvent.click(within(alice).getByRole('button', { name: 'Cancel deletion Alice' }))
  expect(sent(calls, 'DELETE', '/api/workspaces/1/account-deletions/2')).toHaveLength(0)
  await answer('Yes, cancel it')
  await waitFor(() => expect(sent(calls, 'DELETE', '/api/workspaces/1/account-deletions/2')).toHaveLength(1))
  await waitFor(() => expect(within(list).queryByText('Alice')).toBeNull())
})
