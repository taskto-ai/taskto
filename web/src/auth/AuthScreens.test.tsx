// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { setPreferences } from '../i18n'
import { NoWorkspace } from '../shell/NoWorkspace'
import { fakeFetch } from '../test/fakes'
import { InviteScreen, LoginScreen, PaymentReturnScreen, ResetPasswordScreen, SetupScreen } from './AuthScreens'
import { ServerUnreachable } from './ServerUnreachable'

afterEach(async () => {
  cleanup()
  vi.unstubAllGlobals()
  window.history.replaceState(null, '', '/')
  await setPreferences({ user: null })
})

// A field's error, as a screen reader reaches it: the text of the field's description once the field
// is marked invalid (Field says it in its own live region; it is not an alert as well).
async function fieldError(label: string) {
  const field = screen.getByLabelText(label)
  await vi.waitFor(() => expect(field.getAttribute('aria-invalid')).toBe('true'))
  const ids = (field.getAttribute('aria-describedby') ?? '').split(' ').filter(Boolean)
  return ids.map((id) => document.getElementById(id)?.textContent ?? '').join(' ')
}

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
  expect(await fieldError(codeLabel)).toContain('code is wrong')
  expect(screen.queryByRole('alert')).toBeNull()
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
  expect(await fieldError(codeLabel)).toContain('code is wrong')
  expect(screen.queryByRole('alert')).toBeNull()
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

// --- The rework on the component library: what it adds to the screens.

test('the password can be shown and hidden again, and the button says which it does', async () => {
  fakeFetch({ 'GET /api/login/methods': { body: { password: true, sso: [] } } })
  render(<LoginScreen onDone={() => {}} />)
  const password = (await screen.findByLabelText('Password')) as HTMLInputElement
  expect(password.type).toBe('password')
  expect(password.autocomplete).toBe('current-password')
  fireEvent.click(screen.getByRole('button', { name: 'Show password' }))
  expect(password.type).toBe('text')
  fireEvent.click(screen.getByRole('button', { name: 'Hide password' }))
  expect(password.type).toBe('password')
})

test('the fields say what they are to the browser and to password managers', async () => {
  fakeFetch({ 'GET /api/login/methods': { body: { password: true, sso: [] } } })
  render(<LoginScreen onDone={() => {}} />)
  const email = (await screen.findByLabelText('Email')) as HTMLInputElement
  expect(email.type).toBe('email')
  expect(email.autocomplete).toBe('username')
  expect(email.required).toBe(true)
})

test('the button shows it is working while the login is sent, and a second press sends nothing', async () => {
  let answer: (v: { body: unknown }) => void = () => {}
  const calls = fakeFetch({
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/login': () => new Promise((resolve) => (answer = resolve)),
  })
  const onDone = vi.fn()
  render(<LoginScreen onDone={onDone} />)
  await screen.findByLabelText('Password')
  fillLogin()
  const button = screen.getByRole('button', { name: /^Log in/ })
  await vi.waitFor(() => expect(button.getAttribute('aria-busy')).toBe('true'))
  fireEvent.click(button)
  expect(calls.filter((c) => c.path === '/api/login')).toHaveLength(1)
  answer({ body: owner })
  await vi.waitFor(() => expect(onDone).toHaveBeenCalledWith(owner))
})

test('a wrong code is said under the code field, which is marked invalid', async () => {
  fakeFetch({
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/login': { body: challenged },
    'POST /api/login/two-factor': { status: 401, body: { error: 'wrong', code: 'wrong_two_factor_code' } },
  })
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  fillLogin()
  const code = (await screen.findByLabelText(codeLabel)) as HTMLInputElement
  expect(code.autocomplete).toBe('one-time-code')
  fireEvent.change(code, { target: { value: '000000' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  // The message is the field's description, so it is said with the field, and only there.
  expect(await fieldError(codeLabel)).toContain('code is wrong')
  expect(screen.queryByRole('alert')).toBeNull()
})

test('a failure that is not about one field is said for the form, and no field is marked', async () => {
  fakeFetch({
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/login': { status: 401, body: { error: 'wrong email or password', code: 'wrong_credentials' } },
  })
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  fillLogin()
  await screen.findByRole('alert')
  expect(screen.getByLabelText('Email').getAttribute('aria-invalid')).toBeNull()
  expect(screen.getByLabelText('Password').getAttribute('aria-invalid')).toBeNull()
})

test('setup: where the token is found is the field’s help, with the log line’s marker as code', async () => {
  const onDone = vi.fn()
  const calls = fakeFetch({
    'POST /api/setup': (call) =>
      (call.body as { setup_token: string }).setup_token === 'right-token'
        ? { body: owner }
        : { status: 403, body: { error: 'wrong token', code: 'setup_token' } },
  })
  render(<SetupScreen onDone={onDone} />)
  const token = screen.getByLabelText('Setup token') as HTMLInputElement
  const hint = document.getElementById((token.getAttribute('aria-describedby') ?? '').split(' ')[0])
  expect(hint?.textContent).toContain('setup_token=')
  expect(hint?.querySelector('code')?.textContent).toBe('setup_token=')
  expect((screen.getByLabelText('Password') as HTMLInputElement).autocomplete).toBe('new-password')
  screen.getByText('At least 12 characters.')

  fireEvent.change(token, { target: { value: ' wrong-token ' } })
  fireEvent.change(screen.getByLabelText('Your name'), { target: { value: 'Owner' } })
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'owner@example.com' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  fireEvent.click(screen.getByRole('button', { name: 'Create the owner' }))
  expect(await fieldError('Setup token')).toContain('setup token')
  expect(onDone).not.toHaveBeenCalled()

  fireEvent.change(token, { target: { value: ' right-token ' } })
  fireEvent.click(screen.getByRole('button', { name: 'Create the owner' }))
  await vi.waitFor(() => expect(onDone).toHaveBeenCalledWith(owner))
  // The token is sent trimmed, as before.
  expect(calls.at(-1)?.body).toMatchObject({ setup_token: 'right-token', workspace_name: 'Home' })
})

test('a reset whose two passwords differ says so under the second, and sends nothing', async () => {
  const calls = fakeFetch({ 'GET /api/login/methods': { body: both } })
  render(<ResetPasswordScreen token="fake-reset-token" onDone={() => {}} />)
  fireEvent.change(await screen.findByLabelText('New password'), { target: { value: 'fake-new-password-123' } })
  const again = screen.getByLabelText('New password again')
  fireEvent.change(again, { target: { value: 'fake-new-password-124' } })
  expect(await fieldError('New password again')).toContain("The passwords don't match.")
  // Not an alert as well: a live alert on every character typed would be said again and again.
  expect(screen.queryByRole('alert')).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: 'Set the password' }))
  expect(calls.some((c) => c.method === 'POST')).toBe(false)
})

test('before the login methods are known, the logo and a still form are shown, not a word alone', async () => {
  fakeFetch({ 'GET /api/login/methods': () => new Promise(() => {}) })
  render(<LoginScreen onDone={() => {}} />)
  expect(screen.getByRole('img', { name: 'TaskTo.AI' })).toBeTruthy()
  expect(screen.getByRole('main').getAttribute('aria-busy')).toBe('true')
  expect(screen.queryByRole('heading')).toBeNull()
  expect(screen.queryByLabelText('Password')).toBeNull()
})

test('the server unreachable says what happened, and Try again tries again', () => {
  const retry = vi.fn()
  render(<ServerUnreachable error="Failed to fetch" onRetry={retry} />)
  const alert = screen.getByRole('alert')
  expect(alert.textContent).toContain("Can't reach the server")
  expect(alert.textContent).toContain('Failed to fetch')
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
  expect(retry).toHaveBeenCalledOnce()
})

test('the server unreachable and no workspace are the frame’s own form: a display title, no box inside the card', () => {
  render(<ServerUnreachable error="Failed to fetch" onRetry={() => {}} />)
  const title = screen.getByRole('heading', { level: 1, name: "Can't reach the server" })
  expect(title.getAttribute('data-size')).toBe('display')
  expect(document.querySelector('.ui-auth-card .ui-empty')).toBeNull()
  cleanup()

  const none = { user: { id: 1, email: 'owner@example.com', name: 'Owner' }, workspaces: [] }
  render(<NoWorkspace me={none as never} onLogout={() => {}} />)
  const heading = screen.getByRole('heading', { level: 1, name: 'No workspace yet' })
  expect(heading.getAttribute('data-size')).toBe('display')
  expect(document.querySelectorAll('h1')).toHaveLength(1)
  expect(document.querySelector('.ui-auth-card .ui-empty')).toBeNull()
  expect(screen.getByRole('button', { name: 'Log out' })).toBeTruthy()
})

test('an invite for someone signed in names the account, joins with one button, and offers to log out', async () => {
  const onLogout = vi.fn()
  const joined = { ...owner, workspaces: [{ id: 7 }] }
  const onDone = vi.fn()
  const calls = fakeFetch({ 'POST /api/invites/accept': { body: joined } })
  render(<InviteScreen token="fake-invite-token" me={owner as never} onDone={onDone} onLogout={onLogout} />)
  screen.getByText('owner@example.com')
  fireEvent.click(screen.getByRole('button', { name: 'Log out, to use another account' }))
  expect(onLogout).toHaveBeenCalledOnce()
  fireEvent.click(screen.getByRole('button', { name: 'Join the workspace' }))
  await vi.waitFor(() => expect(onDone).toHaveBeenCalledWith(joined, owner))
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ token: 'fake-invite-token' })
})

// --- A password is never carried over shown, and the form's guards each hold on their own.

test('a password shown on the invite’s new-account form is not shown on the log-in form that follows', async () => {
  fakeFetch({
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/invites/accept': { status: 409, body: { error: 'exists', code: 'invite_account_exists' } },
  })
  render(invite())
  fireEvent.change(await screen.findByLabelText('Your name'), { target: { value: 'Owner' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  fireEvent.click(screen.getByRole('button', { name: 'Show password' }))
  expect((screen.getByLabelText('Password') as HTMLInputElement).type).toBe('text')
  fireEvent.click(screen.getByRole('button', { name: 'Create the account' }))
  await screen.findByRole('heading', { name: 'Log in to join' })
  const password = screen.getByLabelText('Password') as HTMLInputElement
  expect(password.type).toBe('password')
  expect(password.value).toBe('')
  expect(password.autocomplete).toBe('current-password')
  screen.getByRole('button', { name: 'Show password' })
})

test('back from the two-factor step, the password is empty and hidden', async () => {
  fakeFetch({
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/login': { body: challenged },
  })
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  fireEvent.click(screen.getByRole('button', { name: 'Show password' }))
  fillLogin()
  await screen.findByRole('heading', { name: 'Two-factor login' })
  fireEvent.click(screen.getByRole('button', { name: 'Back to the password' }))
  const password = (await screen.findByLabelText('Password')) as HTMLInputElement
  expect(password.value).toBe('')
  expect(password.type).toBe('password')
})

test('Show is not a submit button: pressing it in a form sends nothing', async () => {
  const calls = fakeFetch({ 'GET /api/login/methods': { body: { password: true, sso: [] } } })
  render(<LoginScreen onDone={() => {}} />)
  fireEvent.change(await screen.findByLabelText('Email'), { target: { value: 'owner@example.com' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  const show = screen.getByRole('button', { name: 'Show password' }) as HTMLButtonElement
  expect(show.type).toBe('button')
  fireEvent.click(show)
  expect(calls.some((c) => c.method === 'POST')).toBe(false)
})

test('a shown password keeps what it is to a password manager', async () => {
  fakeFetch({ 'GET /api/login/methods': { body: both } })
  render(<ResetPasswordScreen token="fake-reset-token" onDone={() => {}} />)
  const password = (await screen.findByLabelText('New password')) as HTMLInputElement
  fireEvent.click(screen.getAllByRole('button', { name: 'Show password' })[0])
  expect(password.type).toBe('text')
  expect(password.autocomplete).toBe('new-password')
  expect(password.name).toBe('password')
})

test('a reset whose passwords differ: its button is off, and the form refuses on its own too', async () => {
  const calls = fakeFetch({ 'GET /api/login/methods': { body: both } })
  render(<ResetPasswordScreen token="fake-reset-token" onDone={() => {}} />)
  fireEvent.change(await screen.findByLabelText('New password'), { target: { value: 'fake-new-password-123' } })
  fireEvent.change(screen.getByLabelText('New password again'), { target: { value: 'fake-new-password-124' } })
  // The button can't be pressed…
  expect((screen.getByRole('button', { name: 'Set the password' }) as HTMLButtonElement).disabled).toBe(true)
  // …and a submit that comes another way (Enter in a browser that ignores a disabled default) sends nothing.
  const form = screen.getByLabelText('New password').closest('form')
  if (!form) throw new Error('no form')
  fireEvent.submit(form)
  await Promise.resolve()
  expect(calls.some((c) => c.method === 'POST')).toBe(false)
})

test('after a failed submit the focus goes to the field the error is about, or else to the form’s error', async () => {
  fakeFetch({
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/login': (call) =>
      (call.body as { password: string }).password === 'short'
        ? { status: 400, body: { error: 'too short', code: 'password_too_short', params: { min: 12 } } }
        : { status: 401, body: { error: 'wrong email or password', code: 'wrong_credentials' } },
  })
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  fillLogin()
  const alert = await screen.findByRole('alert')
  await vi.waitFor(() => expect(alert.closest('[tabindex="-1"]')).toBe(document.activeElement))

  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'short' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  await vi.waitFor(() => expect(document.activeElement).toBe(screen.getByLabelText('Password')))
})

test('a challenge that ran out is said for the form, which takes the focus: the code field is off', async () => {
  fakeFetch({
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/login': { body: challenged },
    'POST /api/login/two-factor': { status: 401, body: { error: 'gone', code: 'two_factor_challenge_invalid' } },
  })
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  fillLogin()
  fireEvent.change(await screen.findByLabelText(codeLabel), { target: { value: '123456' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  const alert = await screen.findByRole('alert')
  const code = screen.getByLabelText(codeLabel)
  expect(code.getAttribute('aria-invalid')).toBeNull()
  await vi.waitFor(() => expect(alert.closest('[tabindex="-1"]')).toBe(document.activeElement))
})

test('an error about a field goes once that field is edited, and stays while another one is', async () => {
  fakeFetch({ 'POST /api/setup': { status: 403, body: { error: 'wrong token', code: 'setup_token' } } })
  render(<SetupScreen onDone={() => {}} />)
  const token = screen.getByLabelText('Setup token')
  fireEvent.change(token, { target: { value: 'wrong-token' } })
  fireEvent.change(screen.getByLabelText('Your name'), { target: { value: 'Owner' } })
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'owner@example.com' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  fireEvent.click(screen.getByRole('button', { name: 'Create the owner' }))
  await fieldError('Setup token')
  fireEvent.change(screen.getByLabelText('Your name'), { target: { value: 'Owner Two' } })
  expect(token.getAttribute('aria-invalid')).toBe('true')
  fireEvent.change(token, { target: { value: 'right-token' } })
  expect(token.getAttribute('aria-invalid')).toBeNull()
  expect(screen.queryByText(/setup token/i, { selector: '.ui-field-error *' })).toBeNull()
})

// --- One request at a time, whatever submits the form (not only its button).

const formOf = (label: string) => {
  const form = screen.getByLabelText(label).closest('form')
  if (!form) throw new Error(`no form around ${label}`)
  return form
}

test('a login submitted twice while the first is on its way sends one request', async () => {
  let answer: (v: { body: unknown }) => void = () => {}
  const calls = fakeFetch({
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/login': () => new Promise((resolve) => (answer = resolve)),
  })
  const onDone = vi.fn()
  render(<LoginScreen onDone={onDone} />)
  fireEvent.change(await screen.findByLabelText('Email'), { target: { value: 'owner@example.com' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  fireEvent.submit(formOf('Password'))
  fireEvent.submit(formOf('Password'))
  await vi.waitFor(() => expect(calls.filter((c) => c.path === '/api/login')).toHaveLength(1))
  answer({ body: owner })
  await vi.waitFor(() => expect(onDone).toHaveBeenCalledOnce())
  expect(calls.filter((c) => c.path === '/api/login')).toHaveLength(1)
})

test('setup submitted twice while the first is on its way sends one request, and a later try is let through', async () => {
  let answer: (v: { status?: number; body: unknown }) => void = () => {}
  const calls = fakeFetch({ 'POST /api/setup': () => new Promise((resolve) => (answer = resolve)) })
  render(<SetupScreen onDone={() => {}} />)
  fireEvent.change(screen.getByLabelText('Setup token'), { target: { value: 'wrong-token' } })
  fireEvent.change(screen.getByLabelText('Your name'), { target: { value: 'Owner' } })
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'owner@example.com' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  fireEvent.submit(formOf('Setup token'))
  fireEvent.submit(formOf('Setup token'))
  await vi.waitFor(() => expect(calls.filter((c) => c.path === '/api/setup')).toHaveLength(1))
  answer({ status: 403, body: { error: 'wrong token', code: 'setup_token' } })
  await fieldError('Setup token')
  // Settled: the next submit goes.
  fireEvent.submit(formOf('Setup token'))
  await vi.waitFor(() => expect(calls.filter((c) => c.path === '/api/setup')).toHaveLength(2))
})

test('a reset link asked for twice while the first is on its way is asked for once', async () => {
  let answer: (v: { status?: number; body: unknown }) => void = () => {}
  const calls = fakeFetch({
    'GET /api/login/methods': { body: { ...both, reset_by_email: true } },
    'POST /api/password-reset/request': () => new Promise((resolve) => (answer = resolve)),
  })
  render(<LoginScreen onDone={() => {}} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Forgot your password?' }))
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'ana@example.com' } })
  fireEvent.submit(formOf('Email'))
  fireEvent.submit(formOf('Email'))
  await vi.waitFor(() => expect(calls.filter((c) => c.path === '/api/password-reset/request')).toHaveLength(1))
  answer({ status: 202, body: {} })
  await screen.findByText('Check your email')
  expect(calls.filter((c) => c.path === '/api/password-reset/request')).toHaveLength(1)
})

test('after a login answers, a submit while the app is still opening the account sends nothing more', async () => {
  const calls = fakeFetch({
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    'POST /api/login': { body: owner },
  })
  // The app takes its time with the session (its preferences, the offline data): the screen stays.
  const onDone = vi.fn()
  render(<LoginScreen onDone={onDone} />)
  fireEvent.change(await screen.findByLabelText('Email'), { target: { value: 'owner@example.com' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  fireEvent.submit(formOf('Password'))
  await vi.waitFor(() => expect(onDone).toHaveBeenCalledOnce())
  fireEvent.submit(formOf('Password'))
  await new Promise((r) => setTimeout(r))
  expect(calls.filter((c) => c.path === '/api/login')).toHaveLength(1)
})

test('a single sign-on start pressed twice before the page draws it, on one provider or two, starts once', async () => {
  let answer: (v: { body: unknown }) => void = () => {}
  const calls = fakeFetch({
    'GET /api/login/methods': { body: both },
    'POST /api/sso/google/start': () => new Promise((resolve) => (answer = resolve)),
    'POST /api/sso/authentik/start': () => new Promise((resolve) => (answer = resolve)),
  })
  const assign = stubAssign()
  render(<LoginScreen onDone={() => {}} />)
  const google = await screen.findByRole('button', { name: 'Continue with Google' })
  const authentik = screen.getByRole('button', { name: 'Continue with Authentik' })
  // Both presses land before React draws the first one's loading state.
  act(() => {
    google.click()
    google.click()
    authentik.click()
  })
  await vi.waitFor(() => expect(calls.filter((c) => c.path.startsWith('/api/sso/'))).toHaveLength(1))
  answer({ body: { url: 'https://accounts.example.com/authorize?state=fake' } })
  await vi.waitFor(() => expect(assign).toHaveBeenCalledOnce())
  expect(calls.filter((c) => c.path.startsWith('/api/sso/'))).toHaveLength(1)
})

test('the pages a payer returns to say so, call nothing and show nothing of the payment', () => {
  const calls = fakeFetch({})
  const done = render(<PaymentReturnScreen outcome="done" />)
  screen.getByRole('heading', { name: 'Thank you' })
  expect(done.container.textContent).toContain('the provider will confirm it')
  done.unmount()
  render(<PaymentReturnScreen outcome="cancelled" />)
  screen.getByRole('heading', { name: 'Payment cancelled' })
  expect(screen.getByText(/Nothing was charged/)).toBeTruthy()
  expect(calls).toHaveLength(0)
})
