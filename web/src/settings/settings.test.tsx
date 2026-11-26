// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { App } from '../App'
import type { Consent, Me, ProviderAccount, SearchSettings, Space, SpaceMember, Token, Workspace } from '../api'
import { setPreferences } from '../i18n'
import { beginSession, endSession, resetSession } from '../session'
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
  { id: 2, email: 'alice@example.com', name: 'Alice', role: 'member', joined_at: '2026-11-02T00:00:00.000Z' },
  { id: 3, email: 'bob@example.com', name: 'Bob', role: 'member', joined_at: '2026-11-03T00:00:00.000Z' },
]

const sent = (calls: Call[], method: string, path: string) =>
  calls.filter((c) => c.method === method && c.path === path).map((c) => c.body ?? null)

beforeEach(() => {
  window.history.replaceState(null, '', '/')
  localStorage.clear()
})
afterEach(() => {
  cleanup()
  // No session of one test reaches the next.
  resetSession()
  act(() => resetToasts())
  vi.unstubAllGlobals()
})

test('an invite link creates the account and opens the workspace, without the token in the URL', async () => {
  window.history.replaceState(null, '', '/invite#fake-invite-token')
  const calls = fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { status: 401, body: { error: 'not logged in' } },
    'POST /api/invites/accept': {
      status: 201,
      body: {
        user: { id: 5, email: 'new@example.com', name: 'New' },
        workspaces: [{ id: 7, name: 'Team', role: 'member' }],
      },
    },
    'GET /api/workspaces/7/conversations': { body: { conversations: [] } },
  })
  render(<App />)
  await screen.findByRole('heading', { name: 'Join a workspace' })
  fireEvent.change(screen.getByLabelText('Your name'), { target: { value: 'New' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  fireEvent.click(screen.getByRole('button', { name: 'Create the account' }))
  await screen.findByRole('navigation', { name: 'Sections' })
  expect(sent(calls, 'POST', '/api/invites/accept')).toEqual([
    { token: 'fake-invite-token', name: 'New', password: 'fake-password-123' },
  ])
  expect(window.location.pathname).toBe('/w/7/conversations')
  expect(window.location.hash).toBe('')
})

test('an invite for an address with an account logs in, then joins', async () => {
  window.history.replaceState(null, '', '/invite#fake-invite-token')
  const joined: Me = {
    user: { id: 2, email: 'alice@example.com', name: 'Alice' },
    workspaces: [
      { id: 1, name: 'Home', role: 'member' },
      { id: 7, name: 'Team', role: 'member' },
    ],
    can_create_workspace: false,
    is_operator: false,
  }
  const calls = fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { status: 401, body: { error: 'not logged in' } },
    'POST /api/invites/accept': (call) =>
      (call.body as { name?: string }).name
        ? { status: 409, body: { error: 'an account with this email exists: log in, then accept the invite' } }
        : { body: joined },
    'POST /api/login': { body: { ...joined, workspaces: [joined.workspaces[0]] } },
    'GET /api/workspaces/7/conversations': { body: { conversations: [] } },
  })
  render(<App />)
  fireEvent.change(await screen.findByLabelText('Your name'), { target: { value: 'Alice' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  fireEvent.click(screen.getByRole('button', { name: 'Create the account' }))
  await screen.findByRole('heading', { name: 'Log in to join' })
  expect(screen.getByRole('alert').textContent).toMatch(/log in, then accept/)
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'alice@example.com' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-456' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in and join' }))
  await screen.findByRole('navigation', { name: 'Sections' })
  expect(sent(calls, 'POST', '/api/login')).toEqual([{ email: 'alice@example.com', password: 'fake-password-456' }])
  expect(sent(calls, 'POST', '/api/invites/accept').at(-1)).toEqual({ token: 'fake-invite-token' })
  expect(window.location.pathname).toBe('/w/7/conversations')
})

test('signed in, an invite joins the new workspace and goes to it', async () => {
  window.history.replaceState(null, '', '/invite#fake-invite-token')
  const calls = fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: me },
    'POST /api/invites/accept': {
      body: { ...me, workspaces: [...me.workspaces, { id: 9, name: 'Club', role: 'member' }] },
    },
    'GET /api/workspaces/9/conversations': { body: { conversations: [] } },
  })
  render(<App />)
  expect((await screen.findByText('owner@example.com')).tagName).toBe('STRONG')
  fireEvent.click(screen.getByRole('button', { name: 'Join the workspace' }))
  await screen.findByRole('navigation', { name: 'Sections' })
  expect(sent(calls, 'POST', '/api/invites/accept')).toEqual([{ token: 'fake-invite-token' }])
  expect(window.location.pathname).toBe('/w/9/conversations')
})

test('members see the providers, CLI accounts, search, dictation, notifications, password, sessions, data, tokens, storage and about tabs only; admins see them all', async () => {
  fakeFetch({
    'GET /api/workspaces/1/providers': { body: { providers: [] } },
    'GET /api/me/providers': { body: { providers: [] } },
  })
  render(<SettingsPage me={me} workspace={member} tab="members" />)
  const tabs = screen.getByRole('navigation', { name: 'Settings' })
  expect(
    within(tabs)
      .getAllByRole('link')
      .map((a) => a.textContent),
  ).toEqual([
    'Model providers',
    'CLI accounts',
    'Search',
    'Dictation',
    'Storage',
    'Notifications',
    'Password',
    'Sessions',
    'Your data',
    'Access tokens',
    'About',
  ])
  // In three groups: the workspace's, yours and the server's.
  expect(
    within(tabs)
      .getAllByRole('list')
      .map(
        (l) =>
          l.getAttribute('aria-labelledby') &&
          document.getElementById(l.getAttribute('aria-labelledby') ?? '')?.textContent,
      ),
  ).toEqual(['Workspace', 'You', 'Server'])
  // A member asking for an admin's tab gets the first of theirs.
  expect(within(tabs).getByRole('link', { name: 'Model providers' }).getAttribute('aria-current')).toBe('page')
  await screen.findByRole('region', { name: 'Your own accounts' })
  cleanup()
  fakeFetch({
    'GET /api/workspaces/1/members': { body: { members: people } },
    'GET /api/workspaces/1/invites': { body: { invites: [] } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="members" />)
  expect(
    within(screen.getByRole('navigation', { name: 'Settings' }))
      .getAllByRole('link')
      .map((a) => [a.textContent, a.getAttribute('href')]),
  ).toEqual([
    ['General', '/w/1/settings/general'],
    ['Members', '/w/1/settings/members'],
    ['Shared spaces', '/w/1/settings/spaces'],
    ['Audit log', '/w/1/settings/audit'],
    ['Model providers', '/w/1/settings/providers'],
    ['Integrations', '/w/1/settings/integrations'],
    ['Integration approvals', '/w/1/settings/integration-approvals'],
    ['Integration events', '/w/1/settings/integration-events'],
    ['CLI accounts', '/w/1/settings/cli-accounts'],
    ['Search', '/w/1/settings/search'],
    ['Dictation', '/w/1/settings/dictation'],
    ['Storage', '/w/1/settings/storage'],
    ['Notifications', '/w/1/settings/notifications'],
    ['Password', '/w/1/settings/password'],
    ['Sessions', '/w/1/settings/sessions'],
    ['Your data', '/w/1/settings/data'],
    ['Access tokens', '/w/1/settings/tokens'],
    ['About', '/w/1/settings/about'],
  ])
  await screen.findByText('alice@example.com')
})

test("admins set the workspace's default language, which members without a pick get", async () => {
  const calls = fakeFetch({
    'PATCH /api/workspaces/1': (call) => ({ body: { ...admin, ...(call.body as object) } }),
  })
  // A browser that asks for French, which the app doesn't have.
  vi.spyOn(navigator, 'languages', 'get').mockReturnValue(['fr-FR'])
  render(<SettingsPage me={me} workspace={admin} />)
  // General is an admin's first tab.
  const language = await screen.findByRole('combobox', { name: 'Default language' })
  expect((language as HTMLSelectElement).value).toBe('en')
  expect(
    within(language)
      .getAllByRole('option')
      .map((o) => o.textContent),
  ).toEqual(['English', 'Português (Brasil)'])
  fireEvent.change(language, { target: { value: 'pt-BR' } })
  fireEvent.click(screen.getByRole('button', { name: 'Save' }))
  // Without a pick, and with no browser language the app has, the workspace's default applies.
  await screen.findByText('Salvo.')
  expect(screen.getByRole('heading', { name: 'Geral' })).toBeTruthy()
  expect(sent(calls, 'PATCH', '/api/workspaces/1')).toEqual([{ locale: 'pt-BR' }])
  // English, the default, is saved as "": no default picked.
  fireEvent.change(screen.getByRole('combobox', { name: 'Idioma padrão' }), { target: { value: 'en' } })
  fireEvent.click(screen.getByRole('button', { name: 'Salvar' }))
  await screen.findByText('Saved.')
  expect(sent(calls, 'PATCH', '/api/workspaces/1')).toEqual([{ locale: 'pt-BR' }, { locale: '' }])
  vi.restoreAllMocks()
  await act(() => setPreferences({ workspace: null }))
})

test('a default language saved after the tab went (the session ended) changes nothing and says nothing', async () => {
  let answer: () => void = () => {}
  fakeFetch({
    'PATCH /api/workspaces/1': () =>
      new Promise((done) => {
        answer = () => done({ body: { ...admin, locale: 'pt-BR' } })
      }),
  })
  vi.spyOn(navigator, 'languages', 'get').mockReturnValue(['fr-FR'])
  const lang = document.documentElement.lang
  const { unmount } = render(<SettingsPage me={me} workspace={admin} />)
  fireEvent.change(await screen.findByRole('combobox', { name: 'Default language' }), { target: { value: 'pt-BR' } })
  fireEvent.click(screen.getByRole('button', { name: 'Save' }))
  unmount()
  // Long enough for the answer to reach whatever it would change (the language's file is loaded).
  await act(async () => {
    answer()
    await new Promise((r) => setTimeout(r, 200))
  })
  expect(document.documentElement.lang).toBe(lang)
  expect(screen.queryByText(/^(Salvo|Saved)\.$/)).toBeNull()
  vi.restoreAllMocks()
})

test('admins invite, change roles, remove members and withdraw invites', async () => {
  let invites = [
    {
      id: 4,
      email: 'pending@example.com',
      role: 'member',
      created_at: '2026-11-10T00:00:00.000Z',
      expires_at: '2026-11-17T00:00:00.000Z',
    },
  ]
  const calls = fakeFetch({
    'GET /api/workspaces/1/members': { body: { members: people } },
    'GET /api/workspaces/1/invites': () => ({ body: { invites } }),
    'PATCH /api/workspaces/1/members/2': { status: 204 },
    'DELETE /api/workspaces/1/members/3': { status: 204 },
    'POST /api/workspaces/1/invites': (call) => {
      const made = {
        id: 5,
        ...(call.body as { email: string; role: string }),
        created_at: '2026-11-18T00:00:00.000Z',
        expires_at: '2026-11-25T00:00:00.000Z',
      }
      invites = [...invites, made]
      return { status: 201, body: { ...made, token: 'fake-invite-token' } }
    },
    'DELETE /api/workspaces/1/invites/4': { status: 204 },
  })
  render(<SettingsPage me={me} workspace={admin} tab="members" />)

  // Your own role isn't yours to change here, and you can't remove yourself.
  expect((await screen.findByLabelText('Role of Owner')).hasAttribute('disabled')).toBe(true)
  expect(screen.queryByRole('button', { name: 'More actions for Owner' })).toBeNull()

  fireEvent.change(screen.getByLabelText('Role of Alice'), { target: { value: 'admin' } })
  await waitFor(() => expect(sent(calls, 'PATCH', '/api/workspaces/1/members/2')).toEqual([{ role: 'admin' }]))

  // Removing asks first.
  chooseFromMenu('More actions for Bob', 'Remove')
  expect(sent(calls, 'DELETE', '/api/workspaces/1/members/3')).toEqual([])
  expect(within(await screen.findByRole('alertdialog')).getByText('Remove Bob from the workspace?')).toBeTruthy()
  await answer('Yes, remove')
  await waitFor(() => expect(sent(calls, 'DELETE', '/api/workspaces/1/members/3')).toHaveLength(1))

  // An invite's link is shown once, the token in its fragment.
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'new@example.com' } })
  fireEvent.change(screen.getByLabelText('Role'), { target: { value: 'admin' } })
  fireEvent.click(screen.getByRole('button', { name: 'Invite' }))
  const link = (await screen.findByLabelText('Invite link')) as HTMLInputElement
  expect(link.value).toBe(`${window.location.origin}/invite#fake-invite-token`)
  expect(sent(calls, 'POST', '/api/workspaces/1/invites')).toEqual([{ email: 'new@example.com', role: 'admin' }])
  await within(screen.getByRole('list', { name: 'Pending invites' })).findByText('new@example.com')

  fireEvent.click(screen.getByRole('button', { name: 'Withdraw the invite for pending@example.com' }))
  await answer('Yes, withdraw')
  await waitFor(() => expect(sent(calls, 'DELETE', '/api/workspaces/1/invites/4')).toHaveLength(1))
})

test('admins make a password reset link for a member, shown once, never for themselves', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/members': { body: { members: people } },
    'GET /api/workspaces/1/invites': { body: { invites: [] } },
    'POST /api/workspaces/1/members/2/password-reset': {
      status: 201,
      body: { token: 'fake-reset-token', expires_at: '2026-11-20T00:00:00.000Z' },
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="members" />)
  await screen.findByText('alice@example.com')
  expect(screen.queryByRole('button', { name: 'More actions for Owner' })).toBeNull()
  chooseFromMenu('More actions for Alice', 'Reset link')
  const link = (await screen.findByLabelText('Password reset link')) as HTMLInputElement
  expect(link.value).toBe(`${window.location.origin}/reset-password#fake-reset-token`)
  expect(sent(calls, 'POST', '/api/workspaces/1/members/2/password-reset')).toEqual([null])
  expect(screen.getByRole('status').textContent).toContain('alice@example.com')
})

test('with password login off, a member has no reset link action', async () => {
  fakeFetch({
    'GET /api/workspaces/1/members': { body: { members: people } },
    'GET /api/workspaces/1/invites': { body: { invites: [] } },
    'GET /api/login/methods': { body: { password: false, sso: [{ id: 'authentik', name: 'Authentik' }] } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="members" />)
  await screen.findByText('alice@example.com')
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Password reset link for Alice' })).toBeNull())
  fireEvent.click(screen.getByRole('button', { name: 'More actions for Alice' }))
  expect(screen.queryByRole('menuitem', { name: 'Reset link' })).toBeNull()
  expect(screen.getByRole('menuitem', { name: 'Remove' })).toBeTruthy()
})

test('no reset link action until password login is confirmed, nor when the check fails', async () => {
  let reply: (v: { status: number; body: unknown }) => void = () => {}
  const pending = new Promise<{ status: number; body: unknown }>((resolve) => {
    reply = resolve
  })
  fakeFetch({
    'GET /api/workspaces/1/members': { body: { members: people } },
    'GET /api/workspaces/1/invites': { body: { invites: [] } },
    'GET /api/login/methods': () => pending,
  })
  render(<SettingsPage me={me} workspace={admin} tab="members" />)
  await screen.findByText('alice@example.com')
  fireEvent.click(screen.getByRole('button', { name: 'More actions for Alice' }))
  expect(screen.queryByRole('menuitem', { name: 'Reset link' })).toBeNull()
  fireEvent.keyDown(screen.getByRole('menu'), { key: 'Escape' })
  await act(async () => reply({ status: 503, body: { error: 'down' } }))
  fireEvent.click(screen.getByRole('button', { name: 'More actions for Alice' }))
  expect(screen.queryByRole('menuitem', { name: 'Reset link' })).toBeNull()
  expect(screen.getByRole('menuitem', { name: 'Remove' })).toBeTruthy()
})

test("an admin's refused reset link says why", async () => {
  fakeFetch({
    'GET /api/workspaces/1/members': { body: { members: people } },
    'GET /api/workspaces/1/invites': { body: { invites: [] } },
    'POST /api/workspaces/1/members/3/password-reset': {
      status: 403,
      body: { error: "this member also belongs to a workspace you aren't an admin of" },
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="members" />)
  await screen.findByText('bob@example.com')
  chooseFromMenu('More actions for Bob', 'Reset link')
  expect((await screen.findByRole('alert')).textContent).toContain("you aren't an admin of")
  expect(screen.queryByLabelText('Password reset link')).toBeNull()
})

test('everyone changes their own password with the current one; a wrong one is said, not a sign-out', async () => {
  const calls = fakeFetch({
    'GET /api/me/two-factor': { body: { enabled: false, recovery_codes_left: 0 } },
    'POST /api/me/password': (call) =>
      (call.body as { current_password: string }).current_password === 'fake-current-password'
        ? { status: 204 }
        : { status: 403, body: { error: 'the current password is wrong' } },
  })
  const lost = vi.fn()
  window.addEventListener('taskto:session-lost', lost)
  render(<SettingsPage me={me} workspace={member} tab="password" />)
  const fill = (current: string, next: string, again: string) => {
    fireEvent.change(screen.getByLabelText('Current password'), { target: { value: current } })
    fireEvent.change(screen.getByLabelText('New password'), { target: { value: next } })
    fireEvent.change(screen.getByLabelText('New password again'), { target: { value: again } })
  }
  // Two new passwords that differ: said, and nothing is sent.
  fill('fake-current-password', 'fake-new-password', 'fake-other-password')
  // The field says so, and is described by it.
  const again = screen.getByLabelText('New password again')
  expect(again.getAttribute('aria-invalid')).toBe('true')
  expect(document.getElementById(again.getAttribute('aria-describedby') ?? '')?.textContent).toBe(
    "The new passwords don't match.",
  )
  expect((screen.getByRole('button', { name: 'Change the password' }) as HTMLButtonElement).disabled).toBe(true)

  fill('not-the-password', 'fake-new-password', 'fake-new-password')
  fireEvent.click(screen.getByRole('button', { name: 'Change the password' }))
  expect((await screen.findByRole('alert')).textContent).toBe('the current password is wrong')
  expect(lost).not.toHaveBeenCalled()

  fill('fake-current-password', 'fake-new-password', 'fake-new-password')
  fireEvent.click(screen.getByRole('button', { name: 'Change the password' }))
  expect(await screen.findByText(/your other sessions are signed out/)).toBeTruthy()
  expect(sent(calls, 'POST', '/api/me/password')).toEqual([
    { current_password: 'not-the-password', new_password: 'fake-new-password' },
    { current_password: 'fake-current-password', new_password: 'fake-new-password' },
  ])
  // The form is emptied: no password left in it.
  expect((screen.getByLabelText('Current password') as HTMLInputElement).value).toBe('')
  window.removeEventListener('taskto:session-lost', lost)
})

test('a reset link sets the password and signs in, without the token in the URL', async () => {
  window.history.replaceState(null, '', '/reset-password#fake-reset-token')
  const calls = fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'POST /api/password-reset': {
      body: {
        user: { id: 2, email: 'alice@example.com', name: 'Alice' },
        workspaces: [{ id: 1, name: 'Home', role: 'member' }],
      },
    },
    'GET /api/workspaces/1/conversations': { body: { conversations: [] } },
  })
  render(<App />)
  await screen.findByRole('heading', { name: 'Set a new password' })
  fireEvent.change(screen.getByLabelText('New password'), { target: { value: 'fake-new-password' } })
  fireEvent.change(screen.getByLabelText('New password again'), { target: { value: 'fake-typo-password' } })
  // Said with the field (its description), not as an alert on every character typed.
  const again = screen.getByLabelText('New password again')
  expect(again.getAttribute('aria-invalid')).toBe('true')
  const described = (again.getAttribute('aria-describedby') ?? '').split(' ').map((id) => document.getElementById(id))
  expect(described.map((el) => el?.textContent ?? '').join(' ')).toContain("The passwords don't match.")
  fireEvent.change(screen.getByLabelText('New password again'), { target: { value: 'fake-new-password' } })
  fireEvent.click(screen.getByRole('button', { name: 'Set the password' }))
  await screen.findByRole('navigation', { name: 'Sections' })
  expect(sent(calls, 'POST', '/api/password-reset')).toEqual([
    { token: 'fake-reset-token', password: 'fake-new-password' },
  ])
  expect(window.location.pathname).not.toBe('/reset-password')
  expect(window.location.hash).toBe('')
})

test('the password tab, a reset link and the reset screen follow the language', async () => {
  await act(() => setPreferences({ user: 'pt-BR' }))
  fakeFetch({
    'GET /api/workspaces/1/members': { body: { members: people } },
    'GET /api/workspaces/1/invites': { body: { invites: [] } },
    'POST /api/workspaces/1/members/2/password-reset': {
      status: 201,
      body: { token: 'fake-reset-token', expires_at: '2026-11-20T12:00:00.000Z' },
    },
  })
  render(<SettingsPage me={me} workspace={member} tab="password" />)
  expect(screen.getByRole('heading', { name: 'Senha' })).toBeTruthy()
  expect(screen.getByLabelText('Senha atual')).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Alterar a senha' })).toBeTruthy()
  cleanup()

  render(<SettingsPage me={me} workspace={admin} tab="members" />)
  await screen.findByText('alice@example.com')
  chooseFromMenu('Mais ações para Alice', 'Link de redefinição')
  const notice = await screen.findByText(/Envie este link para/)
  expect(notice.querySelector('strong')?.textContent).toBe('Alice')
  expect(notice.textContent).toContain('(alice@example.com)')
  expect(notice.textContent).toContain('2026')
  expect(screen.getByLabelText('Link de redefinição de senha')).toBeTruthy()
  cleanup()

  window.history.replaceState(null, '', '/reset-password#fake-reset-token')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } } })
  render(<App />)
  expect(await screen.findByRole('heading', { name: 'Defina uma nova senha' })).toBeTruthy()
  expect(screen.getByLabelText('Repita a nova senha')).toBeTruthy()
  await act(() => setPreferences({ user: null }))
})

test('a used or expired reset link says so', async () => {
  window.history.replaceState(null, '', '/reset-password#fake-used-token')
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'POST /api/password-reset': {
      status: 404,
      body: { error: 'the reset link is unknown, used or expired: ask an admin for a new one' },
    },
  })
  render(<App />)
  fireEvent.change(await screen.findByLabelText('New password'), { target: { value: 'fake-new-password' } })
  fireEvent.change(screen.getByLabelText('New password again'), { target: { value: 'fake-new-password' } })
  fireEvent.click(screen.getByRole('button', { name: 'Set the password' }))
  expect((await screen.findByRole('alert')).textContent).toContain('ask an admin for a new one')
})

test('admins create, rename and archive shared spaces, and manage who is in them', async () => {
  const team: Space = {
    id: 4,
    workspace_id: 1,
    kind: 'shared',
    name: 'Team',
    archived_at: null,
    access: 'none',
  }
  let spaces: Space[] = [
    { id: 2, workspace_id: 1, kind: 'private', name: 'Private', archived_at: null, role: 'owner', access: 'write' },
    team,
  ]
  let teamMembers: SpaceMember[] = [{ id: 2, email: 'alice@example.com', name: 'Alice', role: 'editor', added_at: '' }]
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces?all=true': () => ({ body: { spaces } }),
    'GET /api/workspaces/1/members': { body: { members: people } },
    'GET /api/workspaces/1/spaces/4/members': () => ({ body: { members: teamMembers } }),
    'GET /api/workspaces/1/spaces/6/members': { body: { members: [] } },
    'POST /api/workspaces/1/spaces': (call) => {
      const made: Space = {
        ...team,
        id: 6,
        name: (call.body as { name: string }).name,
        role: 'editor',
        access: 'write',
      }
      spaces = [...spaces, made]
      return { status: 201, body: made }
    },
    'PATCH /api/workspaces/1/spaces/4': (call) => {
      const body = call.body as { name?: string; archived?: boolean }
      spaces = spaces.map((s) =>
        s.id === 4
          ? { ...s, ...(body.name ? { name: body.name } : {}), archived_at: body.archived ? '2026-11-18' : null }
          : s,
      )
      return { body: spaces.find((s) => s.id === 4) }
    },
    'POST /api/workspaces/1/spaces/4/members': (call) => {
      const b = call.body as { user_id: number; role: 'editor' | 'viewer' }
      const p = people.find((x) => x.id === b.user_id)
      if (p) teamMembers = [...teamMembers, { ...p, role: b.role, added_at: '' }]
      return { status: 204 }
    },
    'PATCH /api/workspaces/1/spaces/4/members/2': { status: 204 },
    'DELETE /api/workspaces/1/spaces/4/members/2': () => {
      teamMembers = teamMembers.filter((m) => m.id !== 2)
      return { status: 204 }
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="spaces" />)
  const list = await screen.findByRole('list', { name: 'Shared spaces' })
  // Private spaces aren't the admin's to manage.
  expect(within(list).queryByText('Private')).toBeNull()
  expect(within(list).getByText('You are not a member')).toBeTruthy()

  // A space is managed in a dialog of its own.
  fireEvent.click(within(list).getByRole('button', { name: 'Manage Team' }))
  const panel = await screen.findByRole('dialog', { name: 'Team' })
  await within(panel).findByText('alice@example.com')

  fireEvent.change(within(panel).getByLabelText('Space role of Alice'), { target: { value: 'viewer' } })
  await waitFor(() =>
    expect(sent(calls, 'PATCH', '/api/workspaces/1/spaces/4/members/2')).toEqual([{ role: 'viewer' }]),
  )

  // Adding offers the workspace's members who aren't in the space yet.
  const who = within(panel).getByLabelText('Add a member') as HTMLSelectElement
  expect([...who.options].map((o) => o.textContent)).toEqual([
    'Choose…',
    'Owner (owner@example.com)',
    'Bob (bob@example.com)',
  ])
  fireEvent.change(who, { target: { value: '1' } })
  fireEvent.change(within(panel).getByLabelText('As'), { target: { value: 'viewer' } })
  fireEvent.click(within(panel).getByRole('button', { name: 'Add' }))
  await waitFor(() =>
    expect(sent(calls, 'POST', '/api/workspaces/1/spaces/4/members')).toEqual([{ user_id: 1, role: 'viewer' }]),
  )
  await within(panel).findByText('owner@example.com')

  fireEvent.click(within(panel).getByRole('button', { name: 'Remove Alice' }))
  await answer('Yes, remove')
  await waitFor(() => expect(within(panel).queryByText('alice@example.com')).toBeNull())

  fireEvent.change(within(panel).getByLabelText('Name'), { target: { value: 'Crew' } })
  fireEvent.click(within(panel).getByRole('button', { name: 'Rename' }))
  const crew = await screen.findByRole('dialog', { name: 'Crew' })
  fireEvent.click(within(crew).getByRole('button', { name: 'Archive' }))
  await answer('Yes, archive')
  await within(crew).findByRole('button', { name: 'Unarchive' })
  expect(sent(calls, 'PATCH', '/api/workspaces/1/spaces/4')).toEqual([{ name: 'Crew' }, { archived: true }])
  fireEvent.click(within(crew).getByRole('button', { name: 'Close' }))
  expect(await within(await screen.findByRole('list', { name: 'Shared spaces' })).findByText('Archived')).toBeTruthy()

  fireEvent.change(screen.getByLabelText('New space'), { target: { value: 'Garden' } })
  fireEvent.click(screen.getByRole('button', { name: 'Create' }))
  // A new space opens to add its members.
  await screen.findByRole('dialog', { name: 'Garden' })
  expect(sent(calls, 'POST', '/api/workspaces/1/spaces')).toEqual([{ name: 'Garden' }])
})

const account = (over: Partial<ProviderAccount>): ProviderAccount => ({
  id: 4,
  scope: 'workspace',
  provider: 'ws1-gateway',
  name: 'gateway',
  display_name: 'Team gateway',
  kind: 'openai-compatible',
  base_url: 'https://llm.example.com/v1',
  api_key: 'set',
  models: [{ id: 'chat-model', name: 'Chat model', context_window: 32768, reasoning: true }],
  created_at: '',
  updated_at: '',
  ...over,
})

test('members read the workspace accounts; everyone adds, edits, tests and removes their own', async () => {
  let mine: ProviderAccount[] = []
  const calls = fakeFetch({
    'GET /api/workspaces/1/providers': { body: { providers: [account({})] } },
    'GET /api/me/providers': () => ({ body: { providers: mine } }),
    'POST /api/me/providers': (call) => {
      const b = call.body as { name: string; base_url: string; api_key?: string; models: { id: string }[] }
      // The API answers "set", never the key.
      const made = account({
        id: 9,
        scope: 'user',
        provider: `u1-${b.name}`,
        name: b.name,
        display_name: b.name,
        base_url: b.base_url,
        api_key: b.api_key ? 'set' : 'not set',
        models: b.models,
      })
      mine = [made]
      return { status: 201, body: made }
    },
    'PATCH /api/me/providers/9': (call) => {
      const b = call.body as { api_key?: string | null; models: { id: string }[] }
      mine = [{ ...mine[0], models: b.models, api_key: b.api_key === null ? 'not set' : mine[0].api_key }]
      return { body: mine[0] }
    },
    'POST /api/me/providers/9/test': { body: { ok: true, missing: ['other-model'] } },
    'DELETE /api/me/providers/9': () => {
      mine = []
      return { status: 204 }
    },
  })
  render(<SettingsPage me={me} workspace={member} tab="providers" />)

  // The workspace's: shown, not editable.
  const shared = await screen.findByRole('region', { name: "This workspace's accounts" })
  await within(shared).findByText('Team gateway')
  expect(within(shared).queryByRole('button')).toBeNull()

  const own = screen.getByRole('region', { name: 'Your own accounts' })
  await within(own).findByText('No accounts yet.')
  fireEvent.click(within(own).getByRole('button', { name: 'Add an account' }))
  const form = screen.getByRole('dialog', { name: 'New account' })
  fireEvent.change(within(form).getByLabelText('Name'), { target: { value: 'home' } })
  fireEvent.change(within(form).getByLabelText('Base URL'), { target: { value: 'http://localhost:11434/v1' } })
  fireEvent.change(within(form).getByLabelText('API key'), { target: { value: 'fake-key-not-real-123' } })
  fireEvent.change(within(form).getByLabelText('Models'), { target: { value: 'chat-model\n\n other-model \n' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Add' }))
  await within(own).findByText('home')
  expect(sent(calls, 'POST', '/api/me/providers')).toEqual([
    {
      name: 'home',
      display_name: '',
      base_url: 'http://localhost:11434/v1',
      api_key: 'fake-key-not-real-123',
      models: [{ id: 'chat-model' }, { id: 'other-model' }],
    },
  ])
  // The key is never on the page.
  expect(document.body.innerHTML).not.toContain('fake-key-not-real-123')

  fireEvent.click(within(own).getByRole('button', { name: 'Test home' }))
  expect((await within(own).findByRole('status')).textContent).toBe(
    "The endpoint answered. It didn't list: other-model.",
  )

  // Editing: the key field starts empty, and leaving it so keeps the stored key.
  chooseFromMenu('More actions for home', 'Edit')
  let edit = screen.getByRole('dialog', { name: 'Edit home' })
  expect((within(edit).getByLabelText('New API key') as HTMLInputElement).value).toBe('')
  fireEvent.change(within(edit).getByLabelText('Models'), { target: { value: 'chat-model' } })
  fireEvent.click(within(edit).getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  expect(sent(calls, 'PATCH', '/api/me/providers/9')[0]).toEqual({
    display_name: 'home',
    base_url: 'http://localhost:11434/v1',
    models: [{ id: 'chat-model' }],
  })

  // Removing the key is a choice of its own.
  chooseFromMenu('More actions for home', 'Edit')
  edit = screen.getByRole('dialog', { name: 'Edit home' })
  fireEvent.click(within(edit).getByLabelText('Remove the stored key'))
  fireEvent.click(within(edit).getByRole('button', { name: 'Save' }))
  await within(own).findByText(/Key not set/)
  expect((sent(calls, 'PATCH', '/api/me/providers/9')[1] as { api_key: unknown }).api_key).toBeNull()

  chooseFromMenu('More actions for home', 'Remove')
  await answer('Yes, remove')
  await within(own).findByText('No accounts yet.')
})

test('editing a workspace account keeps its models settings, and a failed test says why', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/providers': { body: { providers: [account({})] } },
    'GET /api/me/providers': { body: { providers: [] } },
    'PATCH /api/workspaces/1/providers/4': { body: account({}) },
    'POST /api/workspaces/1/providers/4/test': {
      body: { ok: false, error: 'in English', code: 'endpoint_key_refused', params: { status: 401 } },
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="providers" />)
  const shared = await screen.findByRole('region', { name: "This workspace's accounts" })
  fireEvent.click(await within(shared).findByRole('button', { name: 'Test Team gateway' }))
  // In the user's language, from the code.
  expect((await within(shared).findByRole('alert')).textContent).toBe('The endpoint answered 401: check the API key.')

  chooseFromMenu('More actions for Team gateway', 'Edit')
  const edit = screen.getByRole('dialog', { name: 'Edit Team gateway' })
  fireEvent.change(within(edit).getByLabelText('Models'), { target: { value: 'chat-model\nnew-model' } })
  fireEvent.change(within(edit).getByLabelText('New API key'), { target: { value: 'fake-rotated-key-123' } })
  fireEvent.click(within(edit).getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(sent(calls, 'PATCH', '/api/workspaces/1/providers/4')).toEqual([
      {
        display_name: 'Team gateway',
        base_url: 'https://llm.example.com/v1',
        api_key: 'fake-rotated-key-123',
        models: [{ id: 'chat-model', name: 'Chat model', context_window: 32768, reasoning: true }, { id: 'new-model' }],
      },
    ]),
  )
})

const endpoint = { account: 4, provider: 'ws1-gateway', model: 'embed-model', base_url: 'https://llm.example.com/v1' }

test('admins choose the embeddings model, and are told what a change does to consent', async () => {
  let settings: SearchSettings = { embeddings: null, documents: 3, pending: 0 }
  const calls = fakeFetch({
    'GET /api/workspaces/1/search/settings': () => ({ body: settings }),
    'PUT /api/workspaces/1/search/settings': (call) => {
      const e = (call.body as { embeddings: { account: number; model: string } }).embeddings
      if (e.model === 'chat-model') {
        return { status: 400, body: { error: "the account's endpoint didn't return embeddings for this model" } }
      }
      settings = { embeddings: { ...e, provider: 'ws1-gateway' }, documents: 3, pending: 3 }
      return { body: settings }
    },
    'GET /api/workspaces/1/providers': { body: { providers: [account({})] } },
    'GET /api/workspaces/1/spaces': { body: { spaces: [] } },
    'GET /api/workspaces/1/members': { body: { members: people } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="search" />)
  await screen.findByText('No embeddings model: search is full text only.')
  const form = screen.getByRole('form', { name: 'Embeddings model' })
  expect(
    within(form).getByText(/stops semantic search in every space until a member of each space consents/),
  ).toBeTruthy()
  expect(within(form).getByText('https://llm.example.com/v1/embeddings')).toBeTruthy()

  fireEvent.change(within(form).getByLabelText('Model'), { target: { value: 'chat-model' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Save' }))
  expect((await within(form).findByRole('alert')).textContent).toMatch(/didn't return embeddings/)

  fireEvent.change(within(form).getByLabelText('Model'), { target: { value: 'embed-model' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Save' }))
  await screen.findByText(/3 waiting to be embedded/)
  expect(sent(calls, 'PUT', '/api/workspaces/1/search/settings').at(-1)).toEqual({
    embeddings: { account: 4, model: 'embed-model' },
  })
})

test('after a save, the embeddings form shows what the server has, not what it held', async () => {
  // Another admin changes the model just after this one saves: the reload brings theirs.
  let settings: SearchSettings = { embeddings: null, documents: 3, pending: 0 }
  fakeFetch({
    'GET /api/workspaces/1/search/settings': () => ({ body: settings }),
    'PUT /api/workspaces/1/search/settings': (call) => {
      const e = (call.body as { embeddings: { account: number; model: string } }).embeddings
      settings = { embeddings: { ...e, model: 'their-model', provider: 'ws1-gateway' }, documents: 3, pending: 3 }
      return { body: { embeddings: { ...e, provider: 'ws1-gateway' }, documents: 3, pending: 3 } }
    },
    'GET /api/workspaces/1/providers': { body: { providers: [account({})] } },
    'GET /api/workspaces/1/spaces': { body: { spaces: [] } },
    'GET /api/workspaces/1/members': { body: { members: people } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="search" />)
  const form = await screen.findByRole('form', { name: 'Embeddings model' })
  fireEvent.change(within(form).getByLabelText('Model'), { target: { value: 'my-model' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Save' }))
  await waitFor(() => {
    const field = within(screen.getByRole('form', { name: 'Embeddings model' })).getByLabelText(
      'Model',
    ) as HTMLInputElement
    expect(field.value).toBe('their-model')
  })
})

test('consent shows the exact endpoint, names it back, and a changed endpoint must be reviewed again', async () => {
  const spaces = [
    { id: 2, workspace_id: 1, kind: 'private', name: 'Private', archived_at: null, role: 'owner', access: 'write' },
    { id: 4, workspace_id: 1, kind: 'shared', name: 'Team', archived_at: null, role: 'viewer', access: 'read' },
  ]
  let current = endpoint
  let given: Consent = { enabled: false, endpoint: current, given_by: null, given_at: null }
  const team: Consent = { enabled: true, endpoint: current, given_by: 2, given_at: '2026-11-18T14:03:11.204Z' }
  const calls = fakeFetch({
    'GET /api/workspaces/1/search/settings': {
      body: { embeddings: { account: 4, provider: 'ws1-gateway', model: 'embed-model' }, documents: 3, pending: 0 },
    },
    'GET /api/workspaces/1/providers': { body: { providers: [account({})] } },
    'GET /api/workspaces/1/spaces': { body: { spaces } },
    'GET /api/workspaces/1/members': { body: { members: people } },
    'GET /api/workspaces/1/spaces/2/search/embedding': () => ({ body: { ...given, endpoint: current } }),
    'GET /api/workspaces/1/spaces/4/search/embedding': { body: team },
    'PUT /api/workspaces/1/spaces/2/search/embedding': (call) => {
      const b = call.body as { enabled: boolean; endpoint?: typeof endpoint }
      if (b.enabled && b.endpoint?.base_url !== current.base_url) {
        return {
          status: 409,
          body: { error: "the workspace's embeddings endpoint has changed: review it, then consent again" },
        }
      }
      given = b.enabled
        ? { enabled: true, endpoint: current, given_by: 1, given_at: '2026-11-18T15:00:00.000Z' }
        : { enabled: false, endpoint: current, given_by: null, given_at: null }
      return { body: given }
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="search" />)

  const mine = await screen.findByRole('region', { name: 'Private' })
  await within(mine).findByText(/Off\./)
  for (const value of ['Team gateway', 'ws1-gateway', 'embed-model', 'https://llm.example.com/v1']) {
    expect(within(mine).getAllByText(value, { exact: false }).length).toBeGreaterThan(0)
  }

  // The team space is on, by Alice; a viewer can't change it.
  const shared = screen.getByRole('region', { name: 'Team' })
  await within(shared).findByText(/with the consent of Alice/)
  expect(within(shared).queryByRole('button')).toBeNull()
  expect(within(shared).getByText("Only this space's editors can change this.")).toBeTruthy()

  // An admin repoints the account between showing and consenting: refused, and the new one shown.
  current = { ...endpoint, base_url: 'https://elsewhere.example.com/v1' }
  fireEvent.click(within(mine).getByRole('button', { name: 'Consent…' }))
  const ask = within(mine).getByRole('group', { name: 'Consent for Private' })
  expect(ask.textContent).toMatch(/to https:\/\/llm\.example\.com\/v1 for the model embed-model/)
  fireEvent.click(within(ask).getByRole('button', { name: 'Yes, send them there' }))
  expect((await within(mine).findByRole('alert')).textContent).toMatch(/has changed: review it/)
  expect(sent(calls, 'PUT', '/api/workspaces/1/spaces/2/search/embedding')).toEqual([{ enabled: true, endpoint }])
  await within(mine).findByText('https://elsewhere.example.com/v1')

  fireEvent.click(within(mine).getByRole('button', { name: 'Consent…' }))
  fireEvent.click(within(mine).getByRole('button', { name: 'Yes, send them there' }))
  await within(mine).findByText(/On\./)
  expect(sent(calls, 'PUT', '/api/workspaces/1/spaces/2/search/embedding').at(-1)).toEqual({
    enabled: true,
    endpoint: current,
  })

  fireEvent.click(within(mine).getByRole('button', { name: 'Withdraw consent' }))
  await within(mine).findByText(/Off\./)
  expect(sent(calls, 'PUT', '/api/workspaces/1/spaces/2/search/embedding').at(-1)).toEqual({ enabled: false })
})

test('consent given for an earlier endpoint says that sending stopped', async () => {
  fakeFetch({
    'GET /api/workspaces/1/search/settings': {
      body: { embeddings: { account: 4, provider: 'ws1-gateway', model: 'embed-model' }, documents: 0, pending: 0 },
    },
    'GET /api/workspaces/1/providers': { body: { providers: [account({})] } },
    'GET /api/workspaces/1/spaces': {
      body: {
        spaces: [
          {
            id: 2,
            workspace_id: 1,
            kind: 'private',
            name: 'Private',
            archived_at: null,
            role: 'owner',
            access: 'write',
          },
        ],
      },
    },
    'GET /api/workspaces/1/members': { body: { members: people } },
    'GET /api/workspaces/1/spaces/2/search/embedding': {
      body: { enabled: false, endpoint, given_by: 1, given_at: '2026-11-01T00:00:00.000Z' },
    },
  })
  render(<SettingsPage me={me} workspace={member} tab="search" />)
  const mine = await screen.findByRole('region', { name: 'Private' })
  expect((await within(mine).findByText(/Paused\./)).parentElement?.textContent).toMatch(/nothing is sent until/)
  // Members don't choose the model.
  expect(screen.queryByRole('form', { name: 'Embeddings model' })).toBeNull()
})

// Makes a token in the dialog that the page's Create token button opens.
async function createToken(name: string, expires?: string) {
  fireEvent.click(screen.getByRole('button', { name: 'Create token' }))
  const dialog = await screen.findByRole('dialog', { name: 'New access token' })
  fireEvent.change(within(dialog).getByLabelText('Token name'), { target: { value: name } })
  if (expires !== undefined) fireEvent.change(within(dialog).getByLabelText('Expires'), { target: { value: expires } })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Create token' }))
  return dialog
}

// A setup's text, as shown.
const snippet = (name: string) => screen.getByRole('region', { name }).querySelector('pre')?.textContent

const token = (over: Partial<Token>): Token => ({
  id: 3,
  workspace_id: 1,
  name: 'laptop: Claude Code',
  created_at: '2026-11-01T00:00:00.000Z',
  expires_at: null,
  last_used_at: null,
  ...over,
})

test('members list their tokens: name, created, last used, expires', async () => {
  fakeFetch({
    'GET /api/workspaces/1/tokens': {
      body: {
        tokens: [
          token({ last_used_at: '2026-11-17T12:00:00.000Z' }),
          token({ id: 4, name: 'ci script', expires_at: '2026-11-10T00:00:00.000Z' }),
          token({ id: 5, name: 'desktop: Codex', expires_at: '2099-01-01T00:00:00.000Z' }),
        ],
      },
    },
  })
  render(<SettingsPage me={me} workspace={member} tab="tokens" />)
  const list = await screen.findByRole('list', { name: 'Your tokens' })
  const rows = within(list)
    .getAllByRole('listitem')
    .map((li) => li.textContent)
  const d = (t: string) => new Date(t).toLocaleDateString(undefined, { dateStyle: 'medium' })
  expect(rows).toEqual([
    expect.stringMatching(
      `^laptop: Claude CodeCreated ${d('2026-11-01T00:00:00.000Z')} · last used ${d('2026-11-17T12:00:00.000Z')} · never expires`,
    ),
    expect.stringMatching(`^ci scriptCreated .* · never used · expired ${d('2026-11-10T00:00:00.000Z')}`),
    expect.stringMatching(`^desktop: CodexCreated .* · never used · expires ${d('2099-01-01T00:00:00.000Z')}`),
  ])
  // The setup is there before any token, with this server's endpoint and a placeholder.
  const url = `${window.location.origin}/mcp`
  expect(snippet('Codex setup')).toBe(`codex mcp add taskto --url ${url} --bearer-token-env-var TASKTO_TOKEN`)
  expect(snippet('Claude Code setup')).toContain(`taskto ${url}`)
  expect(snippet('Environment variable setup')).toBe("export TASKTO_TOKEN='ttp_…'")
  expect(snippet('Any other MCP client setup')).toContain('"Authorization": "Bearer ttp_…"')
})

test('a new token is shown once, filled into the setup, and gone after leaving the tab', async () => {
  const secret = 'ttp_fake-test-token-not-real'
  let tokens: Token[] = []
  const calls = fakeFetch({
    'GET /api/workspaces/1/tokens': () => ({ body: { tokens } }),
    'POST /api/workspaces/1/tokens': (call) => {
      const made = token({ id: 7, name: (call.body as { name: string }).name })
      tokens = [made]
      return { status: 201, body: { ...made, token: secret } }
    },
    'GET /api/workspaces/1/providers': { body: { providers: [] } },
    'GET /api/me/providers': { body: { providers: [] } },
  })
  const logs = (['log', 'info', 'warn', 'error', 'debug'] as const).map((m) => vi.spyOn(console, m))
  const { rerender } = render(<SettingsPage me={me} workspace={member} tab="tokens" />)
  await screen.findByText('No tokens yet.')

  await createToken('laptop: Claude Code', '')
  expect(((await screen.findByLabelText('New token')) as HTMLInputElement).value).toBe(secret)
  // The dialog closed: the token is on the page.
  expect(screen.queryByRole('dialog')).toBeNull()
  expect(screen.getByRole('status').textContent).toMatch(/won't be shown again/)
  expect(sent(calls, 'POST', '/api/workspaces/1/tokens')).toEqual([{ name: 'laptop: Claude Code' }])
  await within(await screen.findByRole('list', { name: 'Your tokens' })).findByText('laptop: Claude Code')
  expect(snippet('Environment variable setup')).toBe(`export TASKTO_TOKEN='${secret}'`)
  expect(snippet('Any other MCP client setup')).toContain(`"Authorization": "Bearer ${secret}"`)

  // Copy puts it on the clipboard.
  const writeText = vi.fn(async () => {})
  vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })
  fireEvent.click(within(screen.getByRole('status')).getByRole('button', { name: 'Copy' }))
  await within(screen.getByRole('status')).findByRole('button', { name: 'Copied' })
  expect(writeText).toHaveBeenCalledWith(secret)

  // Never kept: not in storage, not logged, and gone once the tab is left.
  expect(JSON.stringify({ ...localStorage, ...sessionStorage })).not.toContain(secret)
  expect(JSON.stringify(logs.flatMap((l) => l.mock.calls))).not.toContain(secret)
  for (const l of logs) l.mockRestore()
  rerender(<SettingsPage me={me} workspace={member} tab="providers" />)
  await screen.findByRole('region', { name: 'Your own accounts' })
  rerender(<SettingsPage me={me} workspace={member} tab="tokens" />)
  await within(await screen.findByRole('list', { name: 'Your tokens' })).findByText('laptop: Claude Code')
  expect(screen.queryByLabelText('New token')).toBeNull()
  expect(document.body.textContent).not.toContain(secret)
  expect(snippet('Environment variable setup')).toBe("export TASKTO_TOKEN='ttp_…'")
})

test('a new token expires when chosen, and Done hides it', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/tokens': { body: { tokens: [] } },
    'POST /api/workspaces/1/tokens': { status: 201, body: { ...token({}), token: 'ttp_fake-test-token' } },
  })
  render(<SettingsPage me={me} workspace={member} tab="tokens" />)
  await screen.findByText('No tokens yet.')
  const before = Date.now()
  await createToken('ci script', '30')
  await screen.findByLabelText('New token')
  const [body] = sent(calls, 'POST', '/api/workspaces/1/tokens') as { name: string; expires_at: string }[]
  expect(body.name).toBe('ci script')
  const days = (Date.parse(body.expires_at) - before) / 86_400_000
  expect(days).toBeGreaterThanOrEqual(30)
  expect(days).toBeLessThan(30.01)
  fireEvent.click(screen.getByRole('button', { name: 'Done' }))
  expect(screen.queryByLabelText('New token')).toBeNull()
  expect(document.body.textContent).not.toContain('ttp_fake-test-token')
})

test('revoking a token asks first', async () => {
  let tokens = [token({}), token({ id: 4, name: 'ci script' })]
  const calls = fakeFetch({
    'GET /api/workspaces/1/tokens': () => ({ body: { tokens } }),
    'DELETE /api/workspaces/1/tokens/4': () => {
      tokens = tokens.filter((t) => t.id !== 4)
      return { status: 204 }
    },
  })
  render(<SettingsPage me={me} workspace={member} tab="tokens" />)
  const list = await screen.findByRole('list', { name: 'Your tokens' })
  fireEvent.click(within(list).getByRole('button', { name: 'Revoke ci script' }))
  expect(sent(calls, 'DELETE', '/api/workspaces/1/tokens/4')).toEqual([])
  // Cancel leaves it be.
  await answer('Cancel')
  expect(screen.queryByRole('alertdialog')).toBeNull()
  expect(sent(calls, 'DELETE', '/api/workspaces/1/tokens/4')).toEqual([])
  fireEvent.click(within(list).getByRole('button', { name: 'Revoke ci script' }))
  expect(
    (await screen.findByRole('alertdialog', { name: 'Revoke ci script? Agents using it lose access at once.' }))
      .textContent,
  ).toBeTruthy()
  await answer('Yes, revoke')
  await waitFor(() => expect(within(list).queryByText('ci script')).toBeNull())
  expect(sent(calls, 'DELETE', '/api/workspaces/1/tokens/4')).toHaveLength(1)
  expect(within(list).getByText('laptop: Claude Code')).toBeTruthy()
})

test("the server's refusals are shown: listing, creating and revoking", async () => {
  fakeFetch({ 'GET /api/workspaces/1/tokens': { status: 500, body: { error: 'database is locked' } } })
  render(<SettingsPage me={me} workspace={member} tab="tokens" />)
  expect((await screen.findByRole('alert')).textContent).toBe('database is locked')
  cleanup()

  fakeFetch({
    'GET /api/workspaces/1/tokens': { body: { tokens: [token({})] } },
    'POST /api/workspaces/1/tokens': { status: 409, body: { error: 'too many tokens: at most 50 per member' } },
    'DELETE /api/workspaces/1/tokens/3': { status: 404, body: { error: 'token not found' } },
  })
  render(<SettingsPage me={me} workspace={member} tab="tokens" />)
  await screen.findByRole('list', { name: 'Your tokens' })
  const dialog = await createToken('one more')
  expect((await within(dialog).findByRole('alert')).textContent).toBe('too many tokens: at most 50 per member')
  expect(screen.queryByLabelText('New token')).toBeNull()
  fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))

  fireEvent.click(screen.getByRole('button', { name: 'Revoke laptop: Claude Code' }))
  await answer('Yes, revoke')
  await screen.findByText('token not found')
})

test('a failed revoke of the new token keeps it on screen, since it still works', async () => {
  const secret = 'ttp_fake-test-token-kept'
  let tokens: Token[] = []
  fakeFetch({
    'GET /api/workspaces/1/tokens': () => ({ body: { tokens } }),
    'POST /api/workspaces/1/tokens': () => {
      const made = token({ id: 7, name: 'ci' })
      tokens = [made]
      return { status: 201, body: { ...made, token: secret } }
    },
    'DELETE /api/workspaces/1/tokens/7': { status: 500, body: { error: 'database is locked' } },
  })
  render(<SettingsPage me={me} workspace={member} tab="tokens" />)
  await screen.findByText('No tokens yet.')
  await createToken('ci')
  expect(((await screen.findByLabelText('New token')) as HTMLInputElement).value).toBe(secret)
  fireEvent.click(await screen.findByRole('button', { name: 'Revoke ci' }))
  await answer('Yes, revoke')
  await screen.findByText('database is locked')
  expect((screen.getByLabelText('New token') as HTMLInputElement).value).toBe(secret)
})

test('a second token waits until the first new one is done with', async () => {
  let tokens: Token[] = []
  const calls = fakeFetch({
    'GET /api/workspaces/1/tokens': () => ({ body: { tokens } }),
    'POST /api/workspaces/1/tokens': () => {
      const made = token({ id: tokens.length + 7, name: `t${tokens.length}` })
      tokens = [...tokens, made]
      return { status: 201, body: { ...made, token: `ttp_fake-${made.id}` } }
    },
  })
  render(<SettingsPage me={me} workspace={member} tab="tokens" />)
  await screen.findByText('No tokens yet.')
  await createToken('first')
  expect(((await screen.findByLabelText('New token')) as HTMLInputElement).value).toBe('ttp_fake-7')
  // While it is on show, another can't replace it: the button that opens the form is off.
  const create = (await screen.findByRole('button', { name: 'Create token' })) as HTMLButtonElement
  expect(create.disabled).toBe(true)
  fireEvent.click(create)
  expect(screen.queryByRole('dialog')).toBeNull()
  expect(sent(calls, 'POST', '/api/workspaces/1/tokens')).toHaveLength(1)
  expect((screen.getByLabelText('New token') as HTMLInputElement).value).toBe('ttp_fake-7')
  fireEvent.click(screen.getByRole('button', { name: 'Done' }))
  expect((screen.getByRole('button', { name: 'Create token' }) as HTMLButtonElement).disabled).toBe(false)
})

// location.assign can't be spied on in jsdom (navigating isn't implemented): the page's location is
// replaced for the test, keeping what the page reads.
function stubAssign() {
  const assign = vi.fn()
  const { pathname, search, hash, href } = window.location
  vi.stubGlobal('location', { pathname, search, hash, href, assign })
  return assign
}

test('the data tab starts the export with a POST, then goes to the address it answers', async () => {
  const assign = stubAssign()
  const calls = fakeFetch({
    'POST /api/workspaces/1/export': { body: { download: '/api/workspaces/1/export/abc', expires_in: 60 } },
  })
  render(<SettingsPage me={me} workspace={member} tab="data" />)
  expect(screen.getByRole('heading', { name: 'Export your data' })).toBeTruthy()
  // Looking at the tab starts nothing: the export begins when the button is pressed.
  expect(calls).toEqual([])
  expect(screen.queryByRole('link', { name: 'Download my data' })).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: 'Download my data' }))
  await waitFor(() => expect(assign).toHaveBeenCalledWith('/api/workspaces/1/export/abc'))
  expect(calls).toEqual([{ method: 'POST', path: '/api/workspaces/1/export', body: {} }])
})

test('the data tab says plainly that a moved app and the conversations come with what is in them', () => {
  fakeFetch({})
  render(<SettingsPage me={me} workspace={member} tab="data" />)
  expect(screen.getByText(/moved there from a shared space/)).toBeTruthy()
  expect(screen.getByText(/teammates' too/)).toBeTruthy()
  expect(screen.getByText(/shared documents the agent read for you/)).toBeTruthy()
})

test.each([
  [409, 'export_running', 'You already have an export running. Wait for it to finish.'],
  [
    429,
    'export_rate_limited',
    'You exported a few times in the last hour, or the server is busy making other exports. Wait a little and try again.',
  ],
])('the data tab shows the translated reason for a %i', async (status, code, text) => {
  const assign = stubAssign()
  fakeFetch({ 'POST /api/workspaces/1/export': { status, body: { error: 'x', code } } })
  render(<SettingsPage me={me} workspace={member} tab="data" />)
  fireEvent.click(screen.getByRole('button', { name: 'Download my data' }))
  expect((await screen.findByRole('alert')).textContent).toBe(text)
  expect(assign).not.toHaveBeenCalled()
  expect((screen.getByRole('button', { name: 'Download my data' }) as HTMLButtonElement).disabled).toBe(false)
})

test('an admin has a second export, for the workspace, and says it holds only the spaces they are in', async () => {
  const assign = stubAssign()
  const calls = fakeFetch({
    'POST /api/workspaces/1/export/workspace': {
      body: { download: '/api/workspaces/1/export/workspace/xyz', expires_in: 60 },
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="data" />)
  expect(screen.getByRole('heading', { name: 'Export the workspace' })).toBeTruthy()
  expect(screen.getByText(/join it first/)).toBeTruthy()
  // Looking at the tab starts nothing.
  expect(calls).toEqual([])
  fireEvent.click(screen.getByRole('button', { name: 'Download the workspace' }))
  await waitFor(() => expect(assign).toHaveBeenCalledWith('/api/workspaces/1/export/workspace/xyz'))
  expect(calls).toEqual([{ method: 'POST', path: '/api/workspaces/1/export/workspace', body: {} }])
})

test('a member has no workspace export on their data tab', () => {
  fakeFetch({})
  render(<SettingsPage me={me} workspace={member} tab="data" />)
  expect(screen.queryByRole('heading', { name: 'Export the workspace' })).toBeNull()
  expect(screen.queryByRole('button', { name: 'Download the workspace' })).toBeNull()
})

test('on a phone, a choice at the top of the page goes to a tab, in the same groups', async () => {
  fakeFetch({
    'GET /api/workspaces/1/providers': { body: { providers: [] } },
    'GET /api/me/providers': { body: { providers: [] } },
  })
  render(<SettingsPage me={me} workspace={member} tab="providers" />)
  const picker = screen.getByRole('combobox', { name: 'Settings page' }) as HTMLSelectElement
  expect(picker.value).toBe('providers')
  expect([...picker.querySelectorAll('optgroup')].map((g) => g.label)).toEqual(['Workspace', 'You', 'Server'])
  // A member is offered only their own tabs.
  expect([...picker.options].map((o) => o.value)).not.toContain('members')
  fireEvent.change(picker, { target: { value: 'tokens' } })
  expect(window.location.pathname).toBe('/w/1/settings/tokens')
})

test("a member's actions are reached by keyboard, and Escape leaves the question unanswered", async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/members': { body: { members: people } },
    'GET /api/workspaces/1/invites': { body: { invites: [] } },
    'GET /api/login/methods': { body: { password: true } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="members" />)
  await screen.findByText('bob@example.com')
  const more = screen.getByRole('button', { name: 'More actions for Bob' })
  // ArrowUp opens the menu on its last item: removing, the dangerous one, after a separator.
  fireEvent.keyDown(more, { key: 'ArrowUp' })
  const remove = await screen.findByRole('menuitem', { name: 'Remove' })
  await waitFor(() => expect(document.activeElement).toBe(remove))
  fireEvent.click(remove)
  const ask = await screen.findByRole('alertdialog', { name: 'Remove Bob from the workspace?' })
  // Cancel has the focus, so Enter by habit doesn't remove anyone.
  expect(document.activeElement?.textContent).toBe('Cancel')
  fireEvent.keyDown(ask, { key: 'Escape' })
  fireEvent(ask, new Event('cancel'))
  await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
  expect(sent(calls, 'DELETE', '/api/workspaces/1/members/3')).toEqual([])
})

test('a cancelled new-token dialog forgets what was typed and what failed', async () => {
  fakeFetch({
    'GET /api/workspaces/1/tokens': { body: { tokens: [token({})] } },
    'POST /api/workspaces/1/tokens': { status: 409, body: { error: 'too many tokens: at most 50 per member' } },
  })
  render(<SettingsPage me={me} workspace={member} tab="tokens" />)
  await screen.findByRole('list', { name: 'Your tokens' })
  const dialog = await createToken('half typed')
  await within(dialog).findByRole('alert')
  fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  fireEvent.click(screen.getByRole('button', { name: 'Create token' }))
  const again = await screen.findByRole('dialog', { name: 'New access token' })
  expect((within(again).getByLabelText('Token name') as HTMLInputElement).value).toBe('')
  expect(within(again).queryByRole('alert')).toBeNull()
})

test('the new-token dialog stays open while the token is being made, so its one showing is not lost', async () => {
  let reply: (v: { status: number; body: unknown }) => void = () => {}
  const pending = new Promise<{ status: number; body: unknown }>((resolve) => {
    reply = resolve
  })
  const calls = fakeFetch({
    'GET /api/workspaces/1/tokens': { body: { tokens: [] } },
    'POST /api/workspaces/1/tokens': () => pending,
  })
  render(<SettingsPage me={me} workspace={member} tab="tokens" />)
  await screen.findByText('No tokens yet.')
  const dialog = await createToken('laptop')
  // On its way: Cancel is off, and Escape (the browser's cancel) doesn't close it.
  await waitFor(() =>
    expect((within(dialog).getByRole('button', { name: 'Cancel' }) as HTMLButtonElement).disabled).toBe(true),
  )
  fireEvent.keyDown(dialog, { key: 'Escape' })
  fireEvent(dialog, new Event('cancel'))
  expect(screen.getByRole('dialog', { name: 'New access token' })).toBeTruthy()
  // A second press of Enter sends nothing more.
  fireEvent.submit(within(dialog).getByLabelText('Token name').closest('form') as HTMLFormElement)
  expect(sent(calls, 'POST', '/api/workspaces/1/tokens')).toHaveLength(1)
  await act(async () =>
    reply({ status: 201, body: { ...token({ id: 7, name: 'laptop' }), token: 'ttp_fake-test-token-pending' } }),
  )
  // The answer is shown, once.
  expect(((await screen.findByLabelText('New token')) as HTMLInputElement).value).toBe('ttp_fake-test-token-pending')
  expect(screen.queryByRole('dialog')).toBeNull()
})

for (const ending of ['the person leaves the tab', 'the session ends'] as const) {
  test(`an export that answers after ${ending} doesn't take the tab to its download`, async () => {
    const assign = stubAssign()
    let answer: (() => void) | null = null
    fakeFetch({
      'POST /api/workspaces/1/export': () =>
        new Promise((done) => {
          answer = () => done({ body: { download: '/api/workspaces/1/export/abc', expires_in: 60 } })
        }),
    })
    beginSession(1)
    const { unmount } = render(<SettingsPage me={me} workspace={member} tab="data" />)
    fireEvent.click(screen.getByRole('button', { name: 'Download my data' }))
    // The export has started: its request is on its way.
    await waitFor(() => expect(answer).not.toBeNull())
    if (ending === 'the person leaves the tab') unmount()
    else endSession()
    await act(async () => answer?.())
    await new Promise((r) => setTimeout(r, 20))
    expect(assign).not.toHaveBeenCalled()
  })
}
