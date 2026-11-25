// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { setPreferences } from '../i18n'
import { fakeFetch } from '../test/fakes'
import { InviteScreen, LoginScreen, ResetPasswordScreen } from './AuthScreens'

afterEach(async () => {
  cleanup()
  vi.unstubAllGlobals()
  window.history.replaceState(null, '', '/')
  await setPreferences({ user: null })
})

const both = {
  password: true,
  sso: [
    { id: 'google', name: 'Google' },
    { id: 'authentik', name: 'Authentik' },
  ],
}
const ssoOnly = { password: false, sso: [{ id: 'authentik', name: 'Authentik' }] }

// location.assign can't be spied on in jsdom (navigating isn't implemented): the page's
// location is replaced for the test, keeping what the screens read.
function stubAssign() {
  const assign = vi.fn()
  const { pathname, search, hash, href } = window.location
  vi.stubGlobal('location', { pathname, search, hash, href, assign })
  return assign
}

test('the login shows a button for each provider above the password form', async () => {
  fakeFetch({ 'GET /api/login/methods': { body: both } })
  render(<LoginScreen onDone={() => {}} />)
  const google = await screen.findByRole('button', { name: 'Continue with Google' })
  const password = screen.getByLabelText('Password')
  screen.getByRole('button', { name: 'Continue with Authentik' })
  expect(google.compareDocumentPosition(password) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  screen.getByText(/Forgot your password/)
})

test('a button starts the sign-in with the page to come back to, then goes to the provider', async () => {
  window.history.replaceState(null, '', '/w/1/boards#card-4')
  const calls = fakeFetch({
    'GET /api/login/methods': { body: both },
    'POST /api/sso/google/start': { body: { url: 'https://accounts.example.com/authorize?state=fake' } },
  })
  const assign = stubAssign()
  render(<LoginScreen onDone={() => {}} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Continue with Google' }))
  await vi.waitFor(() => expect(assign).toHaveBeenCalledWith('https://accounts.example.com/authorize?state=fake'))
  expect(calls.find((c) => c.method === 'POST')).toMatchObject({
    path: '/api/sso/google/start',
    body: { next: '/w/1/boards#card-4' },
  })
})

test('a start that fails says why and leaves the buttons usable', async () => {
  fakeFetch({
    'GET /api/login/methods': { body: both },
    'POST /api/sso/google/start': {
      status: 502,
      body: { error: "the sign-in provider didn't answer", code: 'sso_failed' },
    },
  })
  const assign = stubAssign()
  render(<LoginScreen onDone={() => {}} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Continue with Google' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/Signing in with your provider didn't work/)
  expect(assign).not.toHaveBeenCalled()
  expect((screen.getByRole('button', { name: 'Continue with Google' }) as HTMLButtonElement).disabled).toBe(false)
})

test('with passwords off only the provider buttons are left', async () => {
  fakeFetch({ 'GET /api/login/methods': { body: ssoOnly } })
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByRole('button', { name: 'Continue with Authentik' })
  expect(screen.queryByLabelText('Password')).toBeNull()
  expect(screen.queryByLabelText('Email')).toBeNull()
  expect(screen.queryByText(/Forgot your password/)).toBeNull()
})

test('a server that has no providers, or no such route, shows the password form alone', async () => {
  fakeFetch({ 'GET /api/login/methods': { body: { password: true, sso: [] } } })
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  expect(screen.queryByRole('button', { name: /Continue with/ })).toBeNull()
  cleanup()

  fakeFetch({})
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  expect(screen.queryByRole('button', { name: /Continue with/ })).toBeNull()
})

test('the reason a sign-in failed shows in the language, and leaves the address', async () => {
  window.history.replaceState(null, '', '/?sso_error=sso_no_account')
  fakeFetch({ 'GET /api/login/methods': { body: both } })
  const { unmount } = render(<LoginScreen onDone={() => {}} />)
  expect((await screen.findByRole('alert')).textContent).toBe(
    "There's no account for this email. Ask a workspace admin for an invite.",
  )
  expect(window.location.search).toBe('')
  unmount()

  // A reload doesn't show it again, and a code this web app doesn't know says the sign-in failed.
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  expect(screen.queryByRole('alert')).toBeNull()
  cleanup()
  window.history.replaceState(null, '', '/?sso_error=from_a_newer_server')
  render(<LoginScreen onDone={() => {}} />)
  expect((await screen.findByRole('alert')).textContent).toMatch(/Signing in with your provider didn't work/)

  cleanup()
  await setPreferences({ user: 'pt-BR' })
  window.history.replaceState(null, '', '/?sso_error=sso_expired')
  fakeFetch({ 'GET /api/login/methods': { body: both } })
  render(<LoginScreen onDone={() => {}} />)
  expect((await screen.findByRole('alert')).textContent).toBe(
    'Esse login demorou demais ou já foi usado. Comece de novo.',
  )
  screen.getByRole('button', { name: 'Continuar com Google' })
})

const invite = () => <InviteScreen token="fake-invite-token" me={null} onDone={() => {}} onLogout={() => {}} />

test('an invite offers the providers and comes back to the invite', async () => {
  window.history.replaceState(null, '', '/invite#fake-invite-token')
  const calls = fakeFetch({
    'GET /api/login/methods': { body: both },
    'POST /api/sso/authentik/start': { body: { url: 'https://auth.example.com/authorize?state=fake' } },
  })
  const assign = stubAssign()
  render(invite())
  await screen.findByRole('button', { name: 'Continue with Google' })
  screen.getByLabelText('Your name') // the password form stays, under the buttons
  fireEvent.click(screen.getByRole('button', { name: 'Continue with Authentik' }))
  await vi.waitFor(() => expect(assign).toHaveBeenCalledWith('https://auth.example.com/authorize?state=fake'))
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ next: '/invite#fake-invite-token' })
})

test('an invite with passwords off has no account form', async () => {
  fakeFetch({ 'GET /api/login/methods': { body: ssoOnly } })
  render(invite())
  await screen.findByRole('button', { name: 'Continue with Authentik' })
  expect(screen.queryByLabelText('Password')).toBeNull()
  expect(screen.queryByLabelText('Your name')).toBeNull()
  screen.getByText(/Sign in with your provider/)
})

test('a login-methods failure that is not a 404 says so and a retry fetches again', async () => {
  let answers = 0
  fakeFetch({
    'GET /api/login/methods': () => (answers++ === 0 ? { status: 503, body: { error: 'down' } } : { body: ssoOnly }),
  })
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByText(/Couldn't find out how you can sign in/)
  // No password form is guessed at.
  expect(screen.queryByLabelText('Password')).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
  await screen.findByRole('button', { name: 'Continue with Authentik' })
  expect(screen.queryByText(/Couldn't find out/)).toBeNull()
  expect(screen.queryByLabelText('Password')).toBeNull()
  expect(answers).toBe(2)
})

test('an answer that is not the login methods is an error, not passwords only', async () => {
  for (const body of [
    { password: 'yes', sso: [] },
    { password: true },
    { password: true, sso: [{ id: 1 }] },
    null,
    'nope',
  ]) {
    fakeFetch({ 'GET /api/login/methods': { body } })
    render(<LoginScreen onDone={() => {}} />)
    await screen.findByText(/Couldn't find out how you can sign in/)
    expect(screen.queryByLabelText('Password')).toBeNull()
    screen.getByRole('button', { name: 'Retry' })
    cleanup()
  }
})

test('the invite screen too waits for the methods and retries', async () => {
  let answers = 0
  fakeFetch({
    'GET /api/login/methods': () => (answers++ === 0 ? { status: 500, body: {} } : { body: both }),
  })
  render(<InviteScreen token="tok" me={null} onDone={() => {}} onLogout={() => {}} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Retry' }))
  await screen.findByRole('button', { name: 'Continue with Google' })
})

test('a reset link is not offered a form when password login is off', async () => {
  fakeFetch({ 'GET /api/login/methods': { body: ssoOnly } })
  render(<ResetPasswordScreen token="fake-reset-token" onDone={() => {}} />)
  expect((await screen.findByRole('alert')).textContent).toContain('Password login is turned off')
  expect(screen.queryByLabelText('New password')).toBeNull()
})

test('a reset link with password login on still shows the form', async () => {
  fakeFetch({ 'GET /api/login/methods': { body: both } })
  render(<ResetPasswordScreen token="fake-reset-token" onDone={() => {}} />)
  expect(await screen.findByLabelText('New password')).toBeTruthy()
})

const owner = { user: { id: 1, email: 'owner@example.com', name: 'Owner' }, workspaces: [] }

function fillLogin() {
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'owner@example.com' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
}

test('a login that asks for a code is not logged in until the code is given', async () => {
  const onDone = vi.fn()
  const calls = fakeFetch({
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/login': { body: { two_factor_required: true, challenge: 'fake-challenge', expires_at: 'x' } },
    'POST /api/login/two-factor': (call) =>
      (call.body as { code: string }).code === '123456'
        ? { body: owner }
        : { status: 401, body: { error: 'wrong', code: 'wrong_two_factor_code' } },
  })
  render(<LoginScreen onDone={onDone} />)
  await screen.findByLabelText('Password')
  fillLogin()
  await screen.findByRole('heading', { name: 'Two-factor login' })
  expect(onDone).not.toHaveBeenCalled()
  // The password isn't kept in the page.
  expect(screen.queryByLabelText('Password')).toBeNull()

  const code = screen.getByLabelText('Code from your app, or a recovery code')
  fireEvent.change(code, { target: { value: '000000' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  expect((await screen.findByRole('alert')).textContent).toContain('code is wrong')
  expect(onDone).not.toHaveBeenCalled()

  fireEvent.change(code, { target: { value: '123456' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  await vi.waitFor(() => expect(onDone).toHaveBeenCalledWith(owner))
  expect(calls.filter((c) => c.path === '/api/login/two-factor').map((c) => c.body)).toEqual([
    { challenge: 'fake-challenge', code: '000000' },
    { challenge: 'fake-challenge', code: '123456' },
  ])
})

test('a login whose challenge ran out goes back to the password', async () => {
  fakeFetch({
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/login': { body: { two_factor_required: true, challenge: 'fake-challenge', expires_at: 'x' } },
    'POST /api/login/two-factor': { status: 401, body: { error: 'gone', code: 'two_factor_challenge_invalid' } },
  })
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  fillLogin()
  fireEvent.change(await screen.findByLabelText('Code from your app, or a recovery code'), {
    target: { value: '123456' },
  })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  expect((await screen.findByRole('alert')).textContent).toContain('Log in again with your password')
  expect((screen.getByLabelText('Code from your app, or a recovery code') as HTMLInputElement).disabled).toBe(true)
  fireEvent.click(screen.getByRole('button', { name: 'Log in again' }))
  await screen.findByLabelText('Password')
})

test('a login without two-factor logs in as before', async () => {
  const onDone = vi.fn()
  fakeFetch({
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/login': { body: owner },
  })
  render(<LoginScreen onDone={onDone} />)
  await screen.findByLabelText('Password')
  fillLogin()
  await vi.waitFor(() => expect(onDone).toHaveBeenCalledWith(owner))
})

const challenged = { two_factor_required: true, challenge: 'fake-challenge', expires_at: 'x' }
const codeLabel = 'Code from your app, or a recovery code'

test('a reset link for an account with two-factor login asks for a code and is not logged in before it', async () => {
  const onDone = vi.fn()
  const assign = stubAssign()
  const calls = fakeFetch({
    'GET /api/login/methods': { body: both },
    'POST /api/password-reset': { body: challenged },
    'POST /api/login/two-factor': (call) =>
      (call.body as { code: string }).code === '123456'
        ? { body: owner }
        : { status: 401, body: { error: 'wrong', code: 'wrong_two_factor_code' } },
  })
  render(<ResetPasswordScreen token="fake-reset-token" onDone={onDone} />)
  fireEvent.change(await screen.findByLabelText('New password'), { target: { value: 'fake-new-password-123' } })
  fireEvent.change(screen.getByLabelText('New password again'), { target: { value: 'fake-new-password-123' } })
  fireEvent.click(screen.getByRole('button', { name: 'Set the password' }))
  await screen.findByRole('heading', { name: 'Two-factor login' })
  screen.getByText(/Your new password is set/)
  expect(onDone).not.toHaveBeenCalled()
  expect(screen.queryByLabelText('New password')).toBeNull()

  fireEvent.change(screen.getByLabelText(codeLabel), { target: { value: '000000' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  expect((await screen.findByRole('alert')).textContent).toContain('code is wrong')
  expect(onDone).not.toHaveBeenCalled()
  fireEvent.change(screen.getByLabelText(codeLabel), { target: { value: '123456' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  await vi.waitFor(() => expect(onDone).toHaveBeenCalledWith(owner))
  expect(calls.filter((c) => c.path === '/api/login/two-factor').map((c) => c.body)).toEqual([
    { challenge: 'fake-challenge', code: '000000' },
    { challenge: 'fake-challenge', code: '123456' },
  ])

  // The link is used up: back is the login, not the password form.
  fireEvent.click(screen.getByRole('button', { name: 'Go to the login' }))
  expect(assign).toHaveBeenCalledWith('/')
})

function fillInviteLogin() {
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'owner@example.com' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  fireEvent.click(screen.getByRole('button', { name: /Log in/ }))
}

// An existing account is asked to log in first on an invite: with two-factor login on, the code
// comes before the invite is accepted.
test('an invite taken with an account that has two-factor login asks for the code, then accepts', async () => {
  const joined = { ...owner, workspaces: [{ id: 7 }] }
  const onDone = vi.fn()
  const calls = fakeFetch({
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/invites/accept': (call) =>
      (call.body as { name?: string }).name
        ? { status: 409, body: { error: 'exists', code: 'email_taken' } }
        : { body: joined },
    'POST /api/login': { body: challenged },
    'POST /api/login/two-factor': { body: owner },
  })
  render(<InviteScreen token="fake-invite-token" me={null} onDone={onDone} onLogout={() => {}} />)
  fireEvent.change(await screen.findByLabelText('Your name'), { target: { value: 'Owner' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  fireEvent.click(screen.getByRole('button', { name: 'Create the account' }))
  await screen.findByRole('heading', { name: 'Log in to join' })
  fillInviteLogin()
  await screen.findByRole('heading', { name: 'Two-factor login' })
  // Not accepted, and not told it is signed in, before the code.
  expect(calls.filter((c) => c.path === '/api/invites/accept')).toHaveLength(1)
  expect(onDone).not.toHaveBeenCalled()

  fireEvent.change(screen.getByLabelText(codeLabel), { target: { value: '123456' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  await vi.waitFor(() => expect(onDone).toHaveBeenCalledWith(joined, owner))
  expect(calls.filter((c) => c.path === '/api/invites/accept').at(-1)?.body).toEqual({ token: 'fake-invite-token' })
})

// The code is used up once it passed: if accepting fails after it, trying again accepts, and
// doesn't ask for the code again.
test('an invite whose acceptance fails after the code retries the acceptance only', async () => {
  const joined = { ...owner, workspaces: [{ id: 7 }] }
  const onDone = vi.fn()
  let accepts = 0
  const calls = fakeFetch({
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/invites/accept': () => {
      accepts++
      if (accepts === 1) return { status: 409, body: { error: 'exists', code: 'email_taken' } }
      return accepts === 2 ? { status: 500, body: { error: 'down' } } : { body: joined }
    },
    'POST /api/login': { body: challenged },
    'POST /api/login/two-factor': { body: owner },
  })
  render(<InviteScreen token="fake-invite-token" me={null} onDone={onDone} onLogout={() => {}} />)
  fireEvent.change(await screen.findByLabelText('Your name'), { target: { value: 'Owner' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  fireEvent.click(screen.getByRole('button', { name: 'Create the account' }))
  await screen.findByRole('heading', { name: 'Log in to join' })
  fillInviteLogin()
  fireEvent.change(await screen.findByLabelText(codeLabel), { target: { value: '123456' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  await screen.findByRole('alert')
  expect(onDone).not.toHaveBeenCalled()
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  await vi.waitFor(() => expect(onDone).toHaveBeenCalledWith(joined, owner))
  expect(calls.filter((c) => c.path === '/api/login/two-factor')).toHaveLength(1)
})

test('without email on the server the login says to ask an admin, with no way to ask by email', async () => {
  fakeFetch({ 'GET /api/login/methods': { body: both } })
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  screen.getByText(/Ask a workspace admin for a reset link/)
  expect(screen.queryByRole('button', { name: 'Forgot your password?' })).toBeNull()
})

test('an older server that sends no reset_by_email offers no email either', async () => {
  fakeFetch({ 'GET /api/login/methods': { body: { password: true, sso: [] } } })
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  expect(screen.queryByRole('button', { name: 'Forgot your password?' })).toBeNull()
})

test('forgotten password: the address is asked for, and the answer never says whether an account has it', async () => {
  const calls = fakeFetch({
    'GET /api/login/methods': { body: { ...both, reset_by_email: true } },
    'POST /api/password-reset/request': { status: 202, body: {} },
  })
  const done = vi.fn()
  render(<LoginScreen onDone={done} />)
  fireEvent.change(await screen.findByLabelText('Email'), { target: { value: 'ana@example.com' } })
  fireEvent.click(screen.getByRole('button', { name: 'Forgot your password?' }))
  // The address typed on the login form carries over.
  const email = screen.getByLabelText('Email') as HTMLInputElement
  expect(email.value).toBe('ana@example.com')
  fireEvent.click(screen.getByRole('button', { name: 'Email me a link' }))
  await screen.findByText('Check your email')
  expect(screen.getByText(/If an account has/).textContent).toContain('ana@example.com')
  expect(calls.find((c) => c.method === 'POST')).toMatchObject({
    path: '/api/password-reset/request',
    body: { email: 'ana@example.com' },
  })
  expect(done).not.toHaveBeenCalled()
  fireEvent.click(screen.getByRole('button', { name: 'Back to login' }))
  await screen.findByLabelText('Password')
})

test('forgotten password: too many requests is said in words', async () => {
  fakeFetch({
    'GET /api/login/methods': { body: { ...both, reset_by_email: true } },
    'POST /api/password-reset/request': {
      status: 429,
      body: { error: 'too many reset requests: try again later', code: 'too_many_resets' },
    },
  })
  render(<LoginScreen onDone={() => {}} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Forgot your password?' }))
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'ana@example.com' } })
  fireEvent.click(screen.getByRole('button', { name: 'Email me a link' }))
  expect((await screen.findByRole('alert')).textContent).toBe('Too many reset requests: try again later.')
})
