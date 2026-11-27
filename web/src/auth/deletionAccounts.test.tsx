// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { type Me, SESSION_LOST } from '../api'
import { SettingsPage } from '../settings/SettingsPage'
import { fakeFetch } from '../test/fakes'
import { LoginScreen } from './AuthScreens'
import { ackDeletion, peekDeletion, rememberDeletion } from './deleted'

const PURGE = '2026-12-08T10:00:00.000Z'
const NOTICE = /Your account was deleted/

const leaver: Me = {
  user: { id: 1, email: 'leaver@example.com', name: 'Leaver' },
  workspaces: [{ id: 1, name: 'Home', role: 'member' }],
  can_create_workspace: false,
  is_operator: false,
}
const next: Me = { ...leaver, user: { id: 2, email: 'next@example.com', name: 'Next' } }
const methods = { password: true, sso: [] }
const workspace = { id: 1, name: 'Home', role: 'member' as const }

beforeEach(() => {
  sessionStorage.clear()
  ackDeletion()
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  ackDeletion()
})

async function askDeletion(routes: Parameters<typeof fakeFetch>[0]) {
  let answer!: () => void
  const held = new Promise<void>((resolve) => {
    answer = resolve
  })
  const calls = fakeFetch({
    'GET /api/login/methods': { body: methods },
    'GET /api/me/deletion': {
      body: { grace_days: 14, method: 'password', two_factor: false, fresh: false, blockers: [] },
    },
    'POST /api/me/deletion': async () => {
      await held
      return { status: 202, body: { purge_at: PURGE } }
    },
    ...routes,
  })
  const lost = vi.fn()
  window.addEventListener(SESSION_LOST, lost)
  const page = render(<SettingsPage me={leaver} workspace={workspace} tab="data" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Delete my account…' }))
  const form = await screen.findByRole('form', { name: 'Delete your account' })
  fireEvent.change(within(form).getByLabelText('Your password'), { target: { value: 'fake-test-password' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Delete my account' }))
  await waitFor(() => expect(calls.some((c) => c.path === '/api/me/deletion' && c.method === 'POST')).toBe(true))
  page.unmount()
  return {
    calls,
    arrive: async () => {
      answer()
      await waitFor(() => expect(lost).toHaveBeenCalled())
      window.removeEventListener(SESSION_LOST, lost)
    },
  }
}

function signInAs(email: string) {
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: email } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'fake-password-123' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
}

test('case 1: an answer that arrives after another account signed in is not told to anyone', async () => {
  const onDone = vi.fn()
  const { arrive } = await askDeletion({ 'POST /api/login': { body: next } })
  render(<LoginScreen onDone={onDone} />)
  await screen.findByLabelText('Password')
  signInAs('next@example.com')
  await waitFor(() => expect(onDone).toHaveBeenCalledWith(next))
  cleanup()
  await arrive()
  expect(peekDeletion()).toBeNull()
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  expect(screen.queryByText(NOTICE)).toBeNull()
})

test('case 2: an answer that arrives while another account is on the code step is not told to it', async () => {
  const onDone = vi.fn()
  const { arrive } = await askDeletion({
    'POST /api/login': { body: { two_factor_required: true, challenge: 'fake-challenge', expires_at: 'x' } },
    'POST /api/login/two-factor': { body: next },
  })
  render(<LoginScreen onDone={onDone} />)
  await screen.findByLabelText('Password')
  signInAs('next@example.com')
  await screen.findByRole('heading', { name: 'Two-factor login' })
  await arrive()
  expect(peekDeletion()).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: 'Back to the password' }))
  await screen.findByLabelText('Password')
  expect(screen.queryByText(NOTICE)).toBeNull()
})

test('case 2: an answer that arrives with nobody signing in is told when the screen is up', async () => {
  const { arrive } = await askDeletion({})
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  await act(arrive)
  await screen.findByText(/will be erased for good on .*2026/)
})

test('case 3: a storage that refuses a write still tells the new date, and never an older one kept there', () => {
  sessionStorage.setItem('taskto.accountDeleted', '2026-11-01T10:00:00.000Z')
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
    throw new Error('no storage')
  })
  rememberDeletion(PURGE)
  expect(peekDeletion()).toBe(PURGE)
})

test('case 3: a storage that refuses a removal does not tell the notice again', async () => {
  rememberDeletion(PURGE)
  vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(() => {
    throw new Error('no storage')
  })
  fakeFetch({ 'GET /api/login/methods': { body: methods } })
  const first = render(<LoginScreen onDone={() => {}} />)
  await screen.findByText(/will be erased for good on .*2026/)
  first.unmount()
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  expect(screen.queryByText(NOTICE)).toBeNull()
  expect(peekDeletion()).toBeNull()
})

test('case 3: a notice the storage could not keep is not lost when the page stays', async () => {
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
    throw new Error('no storage')
  })
  fakeFetch({ 'GET /api/login/methods': { body: methods } })
  rememberDeletion(PURGE)
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByText(/will be erased for good on .*2026/)
})

test('case 4: another account that goes to the code step and back is not shown the notice', async () => {
  rememberDeletion(PURGE)
  let state: DocumentVisibilityState = 'hidden'
  vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => state)
  fakeFetch({
    'GET /api/login/methods': { body: methods },
    'POST /api/login': { body: { two_factor_required: true, challenge: 'fake-challenge', expires_at: 'x' } },
  })
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  signInAs('next@example.com')
  await screen.findByRole('heading', { name: 'Two-factor login' })
  fireEvent.click(screen.getByRole('button', { name: 'Back to the password' }))
  await screen.findByLabelText('Password')
  state = 'visible'
  act(() => {
    document.dispatchEvent(new Event('visibilitychange'))
  })
  expect(screen.queryByText(NOTICE)).toBeNull()
  expect(peekDeletion()).toBeNull()
})

test('case 4: a notice that was shown is not shown again on the password form after the code step', async () => {
  rememberDeletion(PURGE)
  fakeFetch({
    'GET /api/login/methods': { body: methods },
    'POST /api/login': { body: { two_factor_required: true, challenge: 'fake-challenge', expires_at: 'x' } },
  })
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByText(/will be erased for good on .*2026/)
  signInAs('next@example.com')
  await screen.findByRole('heading', { name: 'Two-factor login' })
  fireEvent.click(screen.getByRole('button', { name: 'Back to the password' }))
  await screen.findByLabelText('Password')
  expect(screen.queryByText(NOTICE)).toBeNull()
})

test('case 3: an older date the storage kept is not told once the newer one, which it could not keep, was shown', () => {
  sessionStorage.setItem('taskto.accountDeleted', '2026-11-01T10:00:00.000Z')
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
    throw new Error('no storage')
  })
  rememberDeletion(PURGE)
  ackDeletion(PURGE)
  expect(peekDeletion()).toBeNull()
})

test('case 4: another account whose login fails is not shown a notice that was waiting for a page to be seen', async () => {
  rememberDeletion(PURGE)
  let state: DocumentVisibilityState = 'hidden'
  vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => state)
  fakeFetch({
    'GET /api/login/methods': { body: methods },
    'POST /api/login': { status: 401, body: { error: 'wrong', code: 'wrong_credentials' } },
  })
  render(<LoginScreen onDone={() => {}} />)
  await screen.findByLabelText('Password')
  signInAs('next@example.com')
  await screen.findByRole('alert')
  state = 'visible'
  act(() => {
    document.dispatchEvent(new Event('visibilitychange'))
  })
  expect(screen.queryByText(NOTICE)).toBeNull()
})

test('case 3: a notice dropped while the storage could not even be read is not told when it comes back', () => {
  sessionStorage.setItem('taskto.accountDeleted', '2026-12-20T10:00:00.000Z')
  const get = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
    throw new Error('no storage')
  })
  const remove = vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(() => {
    throw new Error('no storage')
  })
  const set = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
    throw new Error('no storage')
  })
  ackDeletion()
  get.mockRestore()
  remove.mockRestore()
  set.mockRestore()
  expect(peekDeletion()).toBeNull()
})

test('case 4: a reset link asked for at the sign-in screen is a submission too: the waiting notice is not told after it', async () => {
  rememberDeletion(PURGE)
  let state: DocumentVisibilityState = 'hidden'
  vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => state)
  fakeFetch({
    'GET /api/login/methods': { body: { ...methods, reset_by_email: true } },
    'POST /api/password-reset/request': { status: 202, body: {} },
  })
  render(<LoginScreen onDone={() => {}} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Forgot your password?' }))
  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'next@example.com' } })
  fireEvent.click(screen.getByRole('button', { name: 'Email me a link' }))
  await screen.findByRole('button', { name: 'Back to login' })
  fireEvent.click(screen.getByRole('button', { name: 'Back to login' }))
  await screen.findByLabelText('Password')
  state = 'visible'
  act(() => {
    document.dispatchEvent(new Event('visibilitychange'))
  })
  expect(screen.queryByText(NOTICE)).toBeNull()
  expect(peekDeletion()).toBeNull()
})

test('case 3: a storage that refuses both a removal and the write over it still does not tell the notice again in this page', () => {
  rememberDeletion(PURGE)
  vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(() => {
    throw new Error('no storage')
  })
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
    throw new Error('no storage')
  })
  ackDeletion(PURGE)
  expect(peekDeletion()).toBeNull()
})

test('case 3: a removal that is refused is written over, so a reload does not tell the notice again either', async () => {
  rememberDeletion(PURGE)
  vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(() => {
    throw new Error('no storage')
  })
  ackDeletion(PURGE)
  expect(sessionStorage.getItem('taskto.accountDeleted')).toBe('')
  vi.resetModules()
  const reloaded = await import('./deleted')
  expect(reloaded.peekDeletion()).toBeNull()
})

test('case 3: an older date the storage kept and cannot remove is not told once the newer one was shown', () => {
  sessionStorage.setItem('taskto.accountDeleted', '2026-11-02T10:00:00.000Z')
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
    throw new Error('no storage')
  })
  vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(() => {
    throw new Error('no storage')
  })
  rememberDeletion(PURGE)
  ackDeletion(PURGE)
  expect(peekDeletion()).toBeNull()
})
