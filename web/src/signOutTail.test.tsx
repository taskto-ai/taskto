// @vitest-environment jsdom

// The tail of a sign-out, as two people sharing one browser meet it: the server is asked first and
// for a bounded time, nothing of this device is touched before it confirms, what is cleared is only
// still the departing account's, and the person always gets somewhere.

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { Me } from './api'
import { backFromSignOut, pendingSignOuts, rememberLogin } from './offline/pendingSignOut'
import { finishUnfinishedSignOuts } from './signOut'
import { fakeFetch } from './test/fakes'

// The sign-out waits a short time here, and the device's cleanup can be made to fail.
const fail = vi.hoisted(() => ({ cleanup: false, activate: false }))
// How long a logout is waited for: long by default, so a logout a test holds is never cut short by
// a loaded machine; a test that wants it to time out says so (`deadline.logout = 0`).
const deadline = vi.hoisted(() => ({ logout: 60_000 }))
vi.mock('./signOut', async (original) => {
  const real = await original<typeof import('./signOut')>()
  return {
    ...real,
    get SIGN_OUT_WAIT_MS() {
      return deadline.logout
    },
  }
})
vi.mock('./offline/runtime', async (original) => {
  const real = await original<typeof import('./offline/runtime')>()
  return {
    ...real,
    deactivate: (...args: Parameters<typeof real.deactivate>) =>
      fail.cleanup ? Promise.reject(new Error('the device refused')) : real.deactivate(...args),
    activate: (...args: Parameters<typeof real.activate>) =>
      fail.activate ? Promise.reject(new Error('the device refused')) : real.activate(...args),
  }
})

const { App } = await import('./App')

const first: Me = {
  user: { id: 1, email: 'first@example.com', name: 'First' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
} as Me
const first_ = first
const second: Me = { ...first, user: { id: 2, email: 'second@example.com', name: 'Second' } } as Me
const pages = {
  'GET /api/setup': { body: { needed: false } },
  'GET /api/workspaces/1/conversations': { body: { conversations: [] } },
}

beforeEach(() => {
  deadline.logout = 60_000
  window.history.replaceState(null, '', '/w/1/conversations')
  fail.cleanup = false
  fail.activate = false
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
  // No answer in time.
  deadline.logout = 0
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
  expect(pendingSignOuts().map((p) => p.user)).toEqual([1])
  expect(calls.some((c) => c.path === '/api/logout')).toBe(true)

  // The next start, with nobody signed in: the cleanup is done this time, and forgotten.
  unmount()
  fail.cleanup = false
  fakeFetch({ ...pages, 'GET /api/me': { status: 401, body: { error: 'not logged in' } } })
  render(<App />)
  await screen.findByRole('heading', { name: 'Log in' })
  await waitFor(() => expect(pendingSignOuts()).toEqual([]))
})

const pendingOf = () => pendingSignOuts().map((p) => `${p.user}:${p.session}`)

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
  window.dispatchEvent(
    new StorageEvent('storage', { key: 'taskto:offline-last', newValue: localStorage.getItem('taskto:offline-last') }),
  )
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

// "Back to the app" and "Try again" are never both in flight: while either waits, the other does
// nothing. So the end is one of two, whichever answers first: signed in with nothing cleared, or
// signed out with the device cleared; never signed out on the server with the device's data left.
test('Back in flight: Try again does nothing, and Back for the same login signs in with nothing cleared', async () => {
  let me: { body: Me } | Promise<{ body: Me }> = { body: { ...first, session: 's1' } }
  let logouts = 0
  fakeFetch({
    ...pages,
    'GET /api/me': () => me,
    'POST /api/logout': () => {
      logouts++
      return logouts === 1 ? { status: 503, body: { error: 'unavailable', code: 'internal' } } : { status: 204 }
    },
  })
  render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  let answer: (() => void) | null = null
  me = new Promise((done) => {
    answer = () => done({ body: { ...first, session: 's1' } })
  })
  fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
  await waitFor(() => expect(answer).not.toBeNull())
  // It says it is working, and Try again can't be pressed.
  const retry = screen.getByRole('button', { name: 'Try again' }) as HTMLButtonElement
  expect(retry.disabled).toBe(true)
  expect(screen.getByRole('button', { name: /Back to the app/ }).getAttribute('aria-busy')).toBe('true')
  fireEvent.click(retry)
  await act(async () => answer?.())
  await screen.findByRole('navigation', { name: 'Sections' })
  expect(logouts).toBe(1)
  expect(pendingOf()).toEqual([])
  expect(localStorage.getItem('taskto:drafts-of')).toBe('1')
})

test('Try again in flight: Back does nothing, and the confirmed logout clears the device', async () => {
  let logouts = 0
  let confirm: (() => void) | null = null
  const calls = fakeFetch({
    ...pages,
    'GET /api/me': { body: { ...first, session: 's1' } },
    'POST /api/logout': () => {
      logouts++
      if (logouts === 1) return { status: 503, body: { error: 'unavailable', code: 'internal' } }
      return new Promise((done) => {
        confirm = () => done({ status: 204 })
      })
    },
  })
  render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  const back = screen.getByRole('button', { name: /Back to the app/ })
  const asked = calls.filter((c) => c.path === '/api/me').length
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
  await waitFor(() => expect(confirm).not.toBeNull())
  // Back, pressed while the logout is in flight: nothing is asked, nothing settled.
  fireEvent.click(back)
  expect(calls.filter((c) => c.path === '/api/me')).toHaveLength(asked)
  expect(pendingOf()).toEqual(['1:s1'])
  await act(async () => confirm?.())
  await screen.findByRole('heading', { name: 'Log in' })
  await waitFor(() => expect(pendingOf()).toEqual([]))
  expect(localStorage.getItem('taskto:drafts-of')).toBeNull()
  expect(calls.filter((c) => c.path === '/api/me')).toHaveLength(asked)
})

test('Back in a tab that took the sign-out up starts the app as a fresh start does', async () => {
  localStorage.setItem(
    'taskto:sign-out-pending',
    JSON.stringify([{ user: 1, session: 's1', phase: 'requested', keep: false, approved: [] }]),
  )
  const fresh = { ...first, session: 's1', user: { ...first.user, time_zone: '' } } as Me
  const calls = fakeFetch({
    ...pages,
    'GET /api/me': { body: fresh },
    'PATCH /api/me': { body: { ...fresh, user: { ...fresh.user, time_zone: 'UTC' } } },
    'POST /api/logout': { status: 500, body: { error: 'internal error', code: 'internal' } },
  })
  render(<App />)
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  expect(localStorage.getItem('taskto:drafts-of')).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
  await screen.findByRole('navigation', { name: 'Sections' })
  // The account's drafts are claimed, and its time zone reported, as at any start.
  expect(localStorage.getItem('taskto:drafts-of')).toBe('1')
  expect(calls.some((c) => c.method === 'PATCH' && c.path === '/api/me')).toBe(true)
})

test('another tab finishing the same sign-out takes this one to the login, not to "The account changed"', async () => {
  let confirm: (() => void) | null = null
  fakeFetch({
    ...pages,
    'GET /api/me': { body: { ...first, session: 's1' } },
    'POST /api/logout': () =>
      new Promise((done) => {
        confirm = () => done({ status: 204 })
      }),
  })
  render(<App />)
  await logOut()
  await waitFor(() => expect(confirm).not.toBeNull())
  // The other tab's cleanup forgets the account: nobody is signed in on this browser now.
  localStorage.removeItem('taskto:offline-last')
  window.dispatchEvent(
    new StorageEvent('storage', { key: 'taskto:offline-last', newValue: localStorage.getItem('taskto:offline-last') }),
  )
  await screen.findByRole('heading', { name: 'Log in' })
  expect(screen.queryByText('The account changed')).toBeNull()
  await act(async () => confirm?.())
  expect(screen.getByRole('heading', { name: 'Log in' })).toBeTruthy()
  // This tab's own logout, confirmed after the page had moved on: its cleanup is still done.
  await waitFor(() => expect(pendingOf()).toEqual([]))
  expect(localStorage.getItem('taskto:drafts-of')).toBeNull()
})

test("a tab that hears its account signed out in another tab forgets the account's pending app submissions", async () => {
  fakeFetch({ ...pages, 'GET /api/me': { body: { ...first, session: 's1' } } })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  // A submission this tab sent had no definite answer.
  sessionStorage.setItem('taskto:operations:1', JSON.stringify([['k', 'op-unanswered']]))
  // Another tab of the account signs out: its cleanup forgets the account.
  localStorage.removeItem('taskto:offline-last')
  window.dispatchEvent(
    new StorageEvent('storage', { key: 'taskto:offline-last', newValue: localStorage.getItem('taskto:offline-last') }),
  )
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(sessionStorage.getItem('taskto:operations:1')).toBeNull()
})

test("a tab that heard another account sign in keeps its own account's pending submissions when that other account signs out", async () => {
  fakeFetch({ ...pages, 'GET /api/me': { body: { ...first, session: 's1' } } })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  sessionStorage.setItem('taskto:operations:1', JSON.stringify([['k', 'op-unanswered']]))
  // Another tab signs the second person in: this one shows the account changed.
  localStorage.setItem('taskto:offline-last', JSON.stringify({ id: 2, name: 'Second' }))
  window.dispatchEvent(
    new StorageEvent('storage', { key: 'taskto:offline-last', newValue: localStorage.getItem('taskto:offline-last') }),
  )
  expect(await screen.findByText('The account changed')).toBeTruthy()
  // Then the second person signs out: not this tab's account.
  localStorage.removeItem('taskto:offline-last')
  window.dispatchEvent(
    new StorageEvent('storage', { key: 'taskto:offline-last', newValue: localStorage.getItem('taskto:offline-last') }),
  )
  await new Promise((r) => setTimeout(r, 0))
  expect(sessionStorage.getItem('taskto:operations:1')).toContain('op-unanswered')
  sessionStorage.removeItem('taskto:operations:1')
})

// Both pressed before the page redraws (a double tap, a slow device): only the first one runs.
for (const first of ['Try again', 'Back to the app'] as const) {
  test(`"${first}" and the other pressed in the same moment: only "${first}" runs`, async () => {
    let logouts = 0
    let confirm: (() => void) | null = null
    let me: { body: Me } | Promise<{ body: Me }> = { body: { ...first_, session: 's1' } }
    let answer: (() => void) | null = null
    const calls = fakeFetch({
      ...pages,
      'GET /api/me': () => me,
      'POST /api/logout': () => {
        logouts++
        if (logouts === 1) return { status: 503, body: { error: 'unavailable', code: 'internal' } }
        return new Promise((done) => {
          confirm = () => done({ status: 204 })
        })
      },
    })
    render(<App />)
    await logOut()
    await screen.findByRole('heading', { name: "Couldn't sign out" })
    me = new Promise((done) => {
      answer = () => done({ body: { ...first_, session: 's1' } })
    })
    const asked = calls.filter((c) => c.path === '/api/me').length
    const retry = screen.getByRole('button', { name: 'Try again' })
    const back = screen.getByRole('button', { name: /Back to the app/ })
    act(() => {
      if (first === 'Try again') {
        retry.click()
        back.click()
      } else {
        back.click()
        retry.click()
      }
    })
    await new Promise((r) => setTimeout(r, 0))
    if (first === 'Try again') {
      // Back asked nothing, and settled nothing: the logout that is in flight decides.
      expect(calls.filter((c) => c.path === '/api/me')).toHaveLength(asked)
      expect(pendingOf()).toEqual(['1:s1'])
      await waitFor(() => expect(confirm).not.toBeNull())
      await act(async () => confirm?.())
      await screen.findByRole('heading', { name: 'Log in' })
      await waitFor(() => expect(pendingOf()).toEqual([]))
    } else {
      // Try again sent nothing: Back's question decides.
      expect(logouts).toBe(1)
      await act(async () => answer?.())
      await screen.findByRole('navigation', { name: 'Sections' })
      expect(logouts).toBe(1)
      expect(pendingOf()).toEqual([])
    }
  })
}

// The step after a confirmed logout (or a 401 to Back's question) is the cleanup, whatever this tab
// did since: a sign-in here meanwhile finishes the sign-out itself, but when the device refused it
// then, this answer finishes it.
for (const step of ['the logout', "Back's question"] as const) {
  test(`${step} confirmed after this tab signed someone else in still clears the device, when that sign-in could not`, async () => {
    let answer: (() => void) | null = null
    const held = <T,>(value: T) =>
      new Promise<T>((done) => {
        answer = () => done(value)
      })
    let me: { body: Me } | Promise<{ status: number; body: unknown }> = { body: { ...first_, session: 's1' } }
    fakeFetch({
      ...pages,
      'GET /api/me': () => me,
      'POST /api/logout': () =>
        step === 'the logout'
          ? held({ status: 204 })
          : { status: 503, body: { error: 'unavailable', code: 'internal' } },
      'GET /api/login/methods': { body: { password: true, sso: [] } },
      'POST /api/login': { body: { ...second, session: 't1' } },
    })
    render(<App />)
    await logOut()
    if (step === "Back's question") {
      await screen.findByRole('heading', { name: "Couldn't sign out" })
      me = held({ status: 401, body: { error: 'not logged in' } })
      fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
    }
    await waitFor(() => expect(answer).not.toBeNull())
    // Another tab finished the same sign-out: nobody is signed in, and this page moves on.
    localStorage.removeItem('taskto:offline-last')
    window.dispatchEvent(
      new StorageEvent('storage', {
        key: 'taskto:offline-last',
        newValue: localStorage.getItem('taskto:offline-last'),
      }),
    )
    // Each step here waits on a start that is slow on a loaded machine.
    const slow = { timeout: 5000 }
    await screen.findByLabelText('Password', {}, slow)
    // The second person signs in here; the device refuses the first person's cleanup at their start.
    fail.cleanup = true
    fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'second@example.com' } })
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
    fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
    await screen.findByRole('button', { name: /^Account: Second/ }, slow)
    expect(pendingOf()).toEqual(['1:s1'])
    fail.cleanup = false
    await act(async () => answer?.())
    // The cleanup waits for the second person's offline start, which is slow on a loaded machine.
    await waitFor(() => expect(pendingOf()).toEqual([]), { timeout: 5000 })
    // The second person is still signed in, and keeps their drafts' claim.
    expect(screen.getByRole('button', { name: /^Account: Second/ })).toBeTruthy()
    expect(localStorage.getItem('taskto:drafts-of')).toBe('2')
  })
}

test('Back that fails while the app starts again leaves the sign-out recorded, and the person on "Couldn\'t sign out"', async () => {
  fakeFetch({
    ...pages,
    'GET /api/me': { body: { ...first_, session: 's1' } },
    'POST /api/logout': { status: 500, body: { error: 'internal error', code: 'internal' } },
  })
  render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  // Starting the app again fails on this device (its offline database).
  fail.activate = true
  fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
  await waitFor(() =>
    expect(
      (screen.getByRole('button', { name: /Back to the app/ }) as HTMLButtonElement).getAttribute('aria-busy'),
    ).not.toBe('true'),
  )
  expect(screen.getByRole('heading', { name: "Couldn't sign out" })).toBeTruthy()
  // Still a sign-out the person chose, not settled: Try again (or the next start) finishes it.
  expect(pendingOf()).toEqual(['1:s1'])
})

test("a tab that hears nobody signed in and then another account, both by the time it looks, forgets its account's pending submissions", async () => {
  fakeFetch({ ...pages, 'GET /api/me': { body: { ...first_, session: 's1' } } })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  sessionStorage.setItem('taskto:operations:1', JSON.stringify([['k', 'op-unanswered']]))
  // The tab was frozen while the first person signed out and the second signed in: both events
  // come at once, and the browser's storage holds the second person by then.
  const second_ = JSON.stringify({ id: 2, name: 'Second' })
  localStorage.setItem('taskto:offline-last', second_)
  window.dispatchEvent(new StorageEvent('storage', { key: 'taskto:offline-last', newValue: null }))
  window.dispatchEvent(new StorageEvent('storage', { key: 'taskto:offline-last', newValue: second_ }))
  expect(await screen.findByText('The account changed')).toBeTruthy()
  expect(sessionStorage.getItem('taskto:operations:1')).toBeNull()
})

test("a logout refused as another account's, after another tab finished the sign-out, leaves the page on the login", async () => {
  let refuse: (() => void) | null = null
  fakeFetch({
    ...pages,
    'GET /api/me': { body: { ...first_, session: 's1' } },
    'POST /api/logout': () =>
      new Promise((done) => {
        refuse = () => done({ status: 409, body: { error: 'another account', code: 'session_account_changed' } })
      }),
  })
  render(<App />)
  await logOut()
  await waitFor(() => expect(refuse).not.toBeNull())
  localStorage.removeItem('taskto:offline-last')
  window.dispatchEvent(new StorageEvent('storage', { key: 'taskto:offline-last', newValue: null }))
  await screen.findByRole('heading', { name: 'Log in' })
  // Someone else signs in in another tab; this tab's logout is answered only now.
  await act(async () => refuse?.())
  await new Promise((r) => setTimeout(r, 20))
  expect(screen.getByRole('heading', { name: 'Log in' })).toBeTruthy()
  expect(screen.queryByText('The account changed')).toBeNull()
})

test('a browser with no offline storage keeps no list of offline accounts after a sign-out', async () => {
  fakeFetch({ ...pages, 'GET /api/me': { body: { ...first_, session: 's1' } }, 'POST /api/logout': { status: 204 } })
  render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: 'Log in' })
  await waitFor(() => expect(pendingOf()).toEqual([]))
  expect(JSON.parse(localStorage.getItem('taskto:offline-users') ?? '[]')).not.toContain(1)
})

test('the invite page with nobody signed in finishes a sign-out left unfinished', async () => {
  localStorage.setItem(
    'taskto:sign-out-pending',
    JSON.stringify([{ user: 1, session: 's1', phase: 'requested', keep: false, approved: [] }]),
  )
  window.history.replaceState(null, '', '/invite#an-invite')
  fakeFetch({ ...pages, 'GET /api/me': { status: 401, body: { error: 'not logged in' } } })
  render(<App />)
  await waitFor(() => expect(pendingOf()).toEqual([]))
})

test("a sign-out refused because the same person signed in again elsewhere doesn't hold that newer login's queue back", async () => {
  fakeFetch({
    ...pages,
    'GET /api/me': { body: { ...first_, session: 's1' } },
    'POST /api/logout': { status: 409, body: { error: 'not this login', code: 'session_account_changed' } },
  })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  // Another tab signed the same person in again: its start remembered the newer login.
  localStorage.setItem('taskto:login', JSON.stringify({ user: 1, session: 's2', at: Date.now() }))
  await logOut()
  expect(await screen.findByText('The account changed')).toBeTruthy()
  // The older login's sign-out is not one that holds the account's data or queue.
  await waitFor(() => expect(pendingOf()).toEqual([]))
})

test("Back in another tab never drops a sign-out while this tab's logout of the same login can still reach the server", async () => {
  let confirm: (() => void) | undefined
  fakeFetch({
    ...pages,
    'GET /api/me': { body: { ...first_, session: 's1' } },
    'POST /api/logout': () =>
      new Promise((done) => {
        confirm = () => done({ status: 204 })
      }),
  })
  render(<App />)
  await logOut()
  await waitFor(() => expect(confirm).toBeDefined())
  // Another tab of the same login takes the sign-out up, gets a definite refusal from the server,
  // and goes back: its own attempt is over, this tab's isn't.
  backFromSignOut(1, 's1')
  expect(pendingSignOuts().find((p) => p.session === 's1')).toMatchObject({ phase: 'backed' })
  // This tab's logout reaches the server after all: the sign-out is finished as the person decided.
  await act(async () => confirm?.())
  await screen.findByRole('heading', { name: 'Log in' })
  await waitFor(() => expect(pendingOf()).toEqual([]))
})

// Two tabs of the same login on "Couldn't sign out"; another tab went back (its own logout was
// refused, so its Back let the sign-out go). This one's Try again is still the person signing out.
async function tryAgainAfterBackElsewhere(logout: () => { status: number; body?: unknown } | Promise<never>) {
  let answer: () => { status: number; body?: unknown } | Promise<never> = () => ({
    status: 503,
    body: { error: 'unavailable', code: 'internal' },
  })
  fakeFetch({ ...pages, 'GET /api/me': { body: { ...first_, session: 's1' } }, 'POST /api/logout': () => answer() })
  render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  backFromSignOut(1, 's1')
  expect(pendingOf()).toEqual([])
  answer = logout
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
}

test('Try again after another tab went back records the sign-out again: a logout with no answer is kept for when it lands', async () => {
  deadline.logout = 0
  await tryAgainAfterBackElsewhere(() => new Promise<never>(() => {}))
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  expect(pendingSignOuts().find((p) => p.session === 's1')).toMatchObject({ unanswered: true })
})

test("Try again after another tab went back keeps the sign-out when the device can't be cleared, for the next start", async () => {
  await tryAgainAfterBackElsewhere(() => {
    fail.cleanup = true
    return { status: 204 }
  })
  await screen.findByRole('heading', { name: 'Log in' })
  await waitFor(() => expect(pendingOf()).toEqual(['1:s1']))
})

test("Back from an older login's sign-out, the browser on a newer login of the same person, lets that older sign-out go", async () => {
  let session = 's1'
  fakeFetch({
    ...pages,
    'GET /api/me': () => ({ body: { ...first_, session } }),
    'POST /api/logout': { status: 503, body: { error: 'unavailable', code: 'internal' } },
  })
  render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  // The same person signed in again in another tab: the browser's login is a newer one.
  session = 's2'
  fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
  expect(await screen.findByText('The account changed')).toBeTruthy()
  // The older login's sign-out holds nothing of the device: it doesn't hold the newer login's queue.
  await waitFor(() => expect(pendingOf()).toEqual([]))
})

test("Back answered 401 after another tab let the sign-out go keeps it when the device can't be cleared", async () => {
  let session: { body: Me } | { status: number; body: unknown } = { body: { ...first_, session: 's1' } }
  fakeFetch({
    ...pages,
    'GET /api/me': () => session,
    'POST /api/logout': { status: 503, body: { error: 'unavailable', code: 'internal' } },
  })
  render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  // Another tab of the same login went back (its own logout refused): the sign-out was let go.
  backFromSignOut(1, 's1')
  expect(pendingOf()).toEqual([])
  // This tab's Back finds the login over, and the device refuses its cleanup.
  session = { status: 401, body: { error: 'not logged in' } }
  fail.cleanup = true
  fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
  await screen.findByRole('heading', { name: 'Log in' })
  await waitFor(() => expect(pendingOf()).toEqual(['1:s1']))
})

test("Try again in a tab of an older login, the browser on a newer login of the same person, doesn't hold that login's queue", async () => {
  fakeFetch({
    ...pages,
    'GET /api/me': { body: { ...first_, session: 's1' } },
    'POST /api/logout': { status: 503, body: { error: 'unavailable', code: 'internal' } },
  })
  render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  // The same person signs in again in another tab: its start settles the older login's sign-out.
  await finishUnfinishedSignOuts({ ...first_, session: 's2' } as Me)
  expect(pendingOf()).toEqual([])
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  await new Promise((r) => setTimeout(r, 0))
  expect(pendingOf()).toEqual([])
})

test("Log out in a tab of an older login, the browser on a newer login of the same person, doesn't hold that login's queue", async () => {
  fakeFetch({
    ...pages,
    'GET /api/me': { body: { ...first_, session: 's1' } },
    'POST /api/logout': { status: 503, body: { error: 'unavailable', code: 'internal' } },
  })
  render(<App />)
  await screen.findByRole('navigation', { name: 'Sections' })
  // The same person signs in again in another tab.
  await finishUnfinishedSignOuts({ ...first_, session: 's2' } as Me)
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  expect(pendingOf()).toEqual([])
})

test("Back from an older login's sign-out, answered for it while a newer login started, lets that older sign-out go", async () => {
  let me: { body: Me } | Promise<{ body: Me }> = { body: { ...first_, session: 's1' } }
  let answer: (() => void) | undefined
  fakeFetch({
    ...pages,
    'GET /api/me': () => me,
    'POST /api/logout': { status: 503, body: { error: 'unavailable', code: 'internal' } },
  })
  render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  me = new Promise((done) => {
    answer = () => done({ body: { ...first_, session: 's1' } })
  })
  fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
  await waitFor(() => expect(answer).toBeDefined())
  // The same person signs in again in another tab, which says so, while Back's answer is on its way.
  rememberLogin(1, 's2', Date.now() + 1)
  const marker = JSON.stringify({ id: 1, name: 'First' })
  localStorage.setItem('taskto:offline-last', marker)
  window.dispatchEvent(new StorageEvent('storage', { key: 'taskto:offline-last', newValue: marker }))
  await act(async () => answer?.())
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await waitFor(() => expect(pendingOf()).toEqual([]))
})

test('Try again refused because a newer login of the same person holds the browser lets that older sign-out go', async () => {
  let logout: { status: number; body?: unknown } = { status: 503, body: { error: 'unavailable', code: 'internal' } }
  fakeFetch({
    ...pages,
    'GET /api/me': { body: { ...first_, session: 's1' } },
    'POST /api/logout': () => logout,
  })
  render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  expect(pendingOf()).toEqual(['1:s1'])
  // A newer login of the same person is the browser's now, learned by a start that asked before this
  // sign-out was recorded (so it didn't settle it).
  rememberLogin(1, 's2', Date.now() + 1)
  logout = { status: 409, body: { error: 'not this login', code: 'session_account_changed' } }
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
  expect(await screen.findByText('The account changed')).toBeTruthy()
  await waitFor(() => expect(pendingOf()).toEqual([]))
})

test("Back from an older login's sign-out with no answer, a newer login of the same person on the browser, lets that older sign-out go", async () => {
  let me: { body: Me } | { status: number; body: unknown } = { body: { ...first_, session: 's1' } }
  fakeFetch({
    ...pages,
    'GET /api/me': () => me,
    'POST /api/logout': { status: 503, body: { error: 'unavailable', code: 'internal' } },
  })
  render(<App />)
  await logOut()
  await screen.findByRole('heading', { name: "Couldn't sign out" })
  // A newer login of the same person holds the browser now; Back gets no answer it can use.
  rememberLogin(1, 's2', Date.now() + 1)
  me = { status: 502, body: null }
  fireEvent.click(screen.getByRole('button', { name: /Back to the app/ }))
  await waitFor(() => expect(pendingOf()).toEqual([]))
})
