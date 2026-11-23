// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { setPreferences } from '../i18n'
import { fakeFetch } from '../test/fakes'
import { InviteScreen, LoginScreen } from './AuthScreens'

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
