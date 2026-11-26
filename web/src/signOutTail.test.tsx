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
  const said = screen.getAllByText(/couldn't be fully cleared/).filter((el) => el.closest('.ui-toast'))
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

test('Back after a refused logout finds the page as the person left it, with what they hadn’t saved', async () => {
  window.history.replaceState(null, '', '/w/1/settings/general')
  fakeFetch({
    ...pages,
    'GET /api/me': { body: { ...first, session: 's1' } },
    'POST /api/logout': { status: 500, body: { error: 'internal error', code: 'internal' } },
  })
  render(<App />)
  // A change not saved yet.
  fireEvent.change(await screen.findByRole('combobox', { name: 'Default language' }), { target: { value: 'pt-BR' } })
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  fireEvent.click(screen.getByRole('button', { name: 'Back to the app' }))
  const language = (await screen.findByRole('combobox', { name: 'Default language' })) as HTMLSelectElement
  expect(language.value).toBe('pt-BR')
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
