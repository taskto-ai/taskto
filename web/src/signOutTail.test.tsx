// @vitest-environment jsdom

// The tail of a sign-out, as two people sharing one browser meet it: the server is asked first and
// for a bounded time, nothing of this device is touched before it confirms, what is cleared is only
// still the departing account's, and the person always gets somewhere.

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { Me } from './api'
import { fakeFetch } from './test/fakes'

// The sign-out waits a short time here, and the device's cleanup can be made to fail.
const fail = vi.hoisted(() => ({ cleanup: false }))
vi.mock('./signOut', async (original) => {
  const real = await original<typeof import('./signOut')>()
  return { ...real, SIGN_OUT_WAIT_MS: 100 }
})
vi.mock('./offline/runtime', async (original) => {
  const real = await original<typeof import('./offline/runtime')>()
  return {
    ...real,
    deactivate: (...args: Parameters<typeof real.deactivate>) =>
      fail.cleanup ? Promise.reject(new Error('the device refused')) : real.deactivate(...args),
  }
})

const { App } = await import('./App')

const first: Me = {
  user: { id: 1, email: 'first@example.com', name: 'First' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
} as Me
const second: Me = { ...first, user: { id: 2, email: 'second@example.com', name: 'Second' } } as Me
const pages = {
  'GET /api/setup': { body: { needed: false } },
  'GET /api/workspaces/1/conversations': { body: { conversations: [] } },
}

beforeEach(() => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fail.cleanup = false
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

async function logOut() {
  fireEvent.click(await screen.findByRole('button', { name: /^Account: First/ }))
  fireEvent.click(within(screen.getByRole('menu')).getByRole('menuitem', { name: 'Log out' }))
}

test("an older tab's logout confirmed after another tab signed someone else in leaves that person's drafts", async () => {
  let answer: (() => void) | null = null
  fakeFetch({
    ...pages,
    'GET /api/me': { body: first },
    'POST /api/logout': () =>
      new Promise((done) => {
        answer = () => done({ status: 204 })
      }),
  })
  render(<App />)
  await logOut()
  await waitFor(() => expect(answer).not.toBeNull())
  // Meanwhile another tab signs the second person in, who starts a draft (the browser's storage,
  // shared by the tabs).
  localStorage.setItem('taskto:drafts-of', '2')
  localStorage.setItem('taskto:builder-start:2:1', 'a draft of the second person')
  await act(async () => answer?.())
  await screen.findByRole('heading', { name: 'Log in' }).catch(() => {})
  expect(localStorage.getItem('taskto:drafts-of')).toBe('2')
  expect(localStorage.getItem('taskto:builder-start:2:1')).toBe('a draft of the second person')
})

test('a logout the server never answers ends at "Couldn\'t sign out", not on the spinner', async () => {
  fakeFetch({ ...pages, 'GET /api/me': { body: first }, 'POST /api/logout': () => new Promise(() => {}) })
  render(<App />)
  await logOut()
  expect(await screen.findByText('Signing out…')).toBeTruthy()
  expect(await screen.findByRole('heading', { name: "Couldn't sign out" })).toBeTruthy()
  expect(screen.getByText(/The server didn't answer in time/)).toBeTruthy()
})

for (const now of ['someone else', 'nobody'] as const) {
  test(`Back after a failed logout, the browser now ${now === 'nobody' ? 'signed out' : "someone else's"}, never reopens the first person's shell`, async () => {
    let signedIn: Me | null = first
    fakeFetch({
      ...pages,
      'GET /api/me': () => (signedIn ? { body: signedIn } : { status: 401, body: { error: 'not logged in' } }),
      'POST /api/logout': { status: 500, body: { error: 'internal error', code: 'internal' } },
    })
    render(<App />)
    await logOut()
    await screen.findByRole('heading', { name: "Couldn't sign out" })
    // The logout went through before the connection dropped, or another tab signed in.
    signedIn = now === 'nobody' ? null : second
    fireEvent.click(screen.getByRole('button', { name: 'Back to the app' }))
    if (now === 'nobody') await screen.findByRole('heading', { name: 'Log in' })
    else expect(await screen.findByText('The account changed')).toBeTruthy()
    expect(screen.queryByRole('navigation', { name: 'Sections' })).toBeNull()
  })
}

test('a cleanup that fails after the server confirmed still reaches the login, says so once, and is finished at the next start', async () => {
  const calls = fakeFetch({ ...pages, 'GET /api/me': { body: first }, 'POST /api/logout': { status: 204 } })
  fail.cleanup = true
  const { unmount } = render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: 'Log in' })
  // Said once, in one toast.
  const said = (await screen.findAllByText(/couldn't be fully cleared/)).filter((el) => el.closest('.ui-toast'))
  expect(said).toHaveLength(1)
  expect(
    JSON.parse(localStorage.getItem('taskto:sign-out-pending') ?? '[]').map((p: { user: number }) => p.user),
  ).toEqual([1])
  expect(calls.some((c) => c.path === '/api/logout')).toBe(true)

  // The next start, with nobody signed in: the cleanup is done this time, and forgotten.
  unmount()
  fail.cleanup = false
  fakeFetch({ ...pages, 'GET /api/me': { status: 401, body: { error: 'not logged in' } } })
  render(<App />)
  await screen.findByRole('heading', { name: 'Log in' })
  await waitFor(() => expect(localStorage.getItem('taskto:sign-out-pending')).toBeNull())
})

const pendingOf = () =>
  (JSON.parse(localStorage.getItem('taskto:sign-out-pending') ?? '[]') as { user: number; session: string }[]).map(
    (p) => `${p.user}:${p.session}`,
  )

test('Back after a refused logout, the browser still on the same login, starts the app afresh and drops the sign-out', async () => {
  fakeFetch({
    ...pages,
    'GET /api/me': { body: { ...first, session: 's1' } },
    'POST /api/logout': { status: 500, body: { error: 'internal error', code: 'internal' } },
  })
  render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  expect(pendingOf()).toEqual(['1:s1'])
  fireEvent.click(screen.getByRole('button', { name: 'Back to the app' }))
  await screen.findByRole('navigation', { name: 'Sections' })
  expect(pendingOf()).toEqual([])
})

test('another tab starting with the login that is signing out takes the sign-out up: it is not cleared, and goes on', async () => {
  // A sign-out certain in a tab that closed (or still finishing in another one): this login's.
  localStorage.setItem(
    'taskto:sign-out-pending',
    JSON.stringify([{ user: 1, session: 's1', phase: 'requested', keep: false, approved: [] }]),
  )
  let answer: (() => void) | null = null
  const calls = fakeFetch({
    ...pages,
    'GET /api/me': { body: { ...first, session: 's1' } },
    'POST /api/logout': () =>
      new Promise((done) => {
        answer = () => done({ status: 204 })
      }),
  })
  render(<App />)
  expect(await screen.findByText('Signing out…')).toBeTruthy()
  await waitFor(() => expect(answer).not.toBeNull())
  // Not signed in again, and the sign-out is still recorded while the server is asked.
  expect(screen.queryByRole('navigation', { name: 'Sections' })).toBeNull()
  expect(pendingOf()).toEqual(['1:s1'])
  // For that login only.
  expect(calls.find((c) => c.path === '/api/logout')?.body).toEqual({ session: 's1' })
  await act(async () => answer?.())
  await screen.findByRole('heading', { name: 'Log in' })
  await waitFor(() => expect(pendingOf()).toEqual([]))
})

test('Try again after the first answer was lost, the browser on a newer login by now, revokes nothing and says the account changed', async () => {
  let logout: { status: number; body: unknown } = { status: 500, body: { error: 'internal error', code: 'internal' } }
  const calls = fakeFetch({
    ...pages,
    'GET /api/me': { body: { ...first, session: 's1' } },
    'POST /api/logout': () => logout,
  })
  render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  // Another tab signed the same person in again: the server's login is a newer one, which this
  // sign-out must not end. The server refuses it.
  logout = { status: 409, body: { error: 'not this login', code: 'session_account_changed' } }
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
  expect(await screen.findByText('The account changed')).toBeTruthy()
  const sent = calls.filter((c) => c.path === '/api/logout').map((c) => c.body)
  expect(sent).toEqual([{ session: 's1' }, { session: 's1' }])
})

test("Back's answer is not taken when another tab signed in while it was asked, even as the same account", async () => {
  let me: Promise<{ body: Me }> | { body: Me } = { body: { ...first, session: 's1' } }
  fakeFetch({
    ...pages,
    'GET /api/me': () => me,
    'POST /api/logout': { status: 500, body: { error: 'internal error', code: 'internal' } },
  })
  render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  let answer: (() => void) | null = null
  me = new Promise((done) => {
    answer = () => done({ body: { ...first, session: 's1' } })
  })
  fireEvent.click(screen.getByRole('button', { name: 'Back to the app' }))
  await waitFor(() => expect(answer).not.toBeNull())
  // Meanwhile another tab signs the same account in (its marker is written again).
  localStorage.setItem('taskto:offline-last', JSON.stringify({ id: 1, name: 'First' }))
  window.dispatchEvent(new StorageEvent('storage', { key: 'taskto:offline-last' }))
  await act(async () => answer?.())
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(screen.queryByRole('navigation', { name: 'Sections' })).toBeNull()
})

test('Back to the same account but another login (made meanwhile) does not reopen the shell', async () => {
  let login = 's1'
  fakeFetch({
    ...pages,
    'GET /api/me': () => ({ body: { ...first, session: login } }),
    'POST /api/logout': { status: 500, body: { error: 'internal error', code: 'internal' } },
  })
  render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  // The same person signed in again in another tab: a new login, another session.
  login = 's2'
  fireEvent.click(screen.getByRole('button', { name: 'Back to the app' }))
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(screen.queryByRole('navigation', { name: 'Sections' })).toBeNull()
})
