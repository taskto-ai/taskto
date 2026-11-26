// @vitest-environment jsdom

// The two pages a link from outside the app opens: a push's /notification/{id} and an email's
// confirmation link. For a signed-in person they are pages of the frame (the sidebar, the skip
// link, <main>); someone signed out gets the sign-in screen alone, as for any address.

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { App } from '../App'
import type { Me } from '../api'
import { EMAIL_CONFIRM_API, EMAIL_CONFIRM_PATH } from '../notifications/api'
import { navigate } from '../router'
import { fakeFetch } from '../test/fakes'
import { resetToasts } from '../ui/Toast'
import { Shell } from './Shell'

const me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
} as Me

beforeEach(() => {
  resetToasts()
  localStorage.clear()
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  window.history.replaceState(null, '', '/')
})

// The frame around a page, as a screen reader finds it.
function frame() {
  return {
    nav: screen.queryByRole('navigation', { name: 'Sections' }),
    main: screen.queryByRole('main'),
    skip: screen.queryByRole('button', { name: 'Skip to the content' }),
  }
}

test('signed in, a notification’s link is a page of the frame, its address kept, the inbox marked', async () => {
  window.history.replaceState(null, '', '/notification/9')
  fakeFetch({ 'GET /api/me/notifications/9': { status: 404, body: { error: 'not found', code: 'not_found' } } })
  render(<Shell me={me} onLogout={() => {}} />)
  const { nav, main, skip } = frame()
  expect(nav).toBeTruthy()
  expect(skip).toBeTruthy()
  expect(within(main as HTMLElement).getByRole('heading', { level: 1, name: 'Notification' })).toBeTruthy()
  expect(await within(main as HTMLElement).findByRole('alert')).toBeTruthy()
  expect(
    within(nav as HTMLElement)
      .getByRole('link', { name: /Notifications/ })
      .getAttribute('aria-current'),
  ).toBe('page')
  // The frame doesn't take the address for an unknown route and rewrite it.
  expect(window.location.pathname).toBe('/notification/9')
})

test('signed in, the email confirmation is a page of the frame; the request is made once, the token leaves at once', async () => {
  window.history.replaceState(null, '', `${EMAIL_CONFIRM_PATH}#a-token-for-the-test`)
  const calls = fakeFetch({ [`POST ${EMAIL_CONFIRM_API}`]: { body: {} } })
  render(<Shell me={me} onLogout={() => {}} />)
  expect(window.location.hash).toBe('')
  const { nav, main } = frame()
  expect(nav).toBeTruthy()
  expect(
    within(main as HTMLElement).getByRole('heading', { level: 1, name: 'Confirm your email address' }),
  ).toBeTruthy()
  await within(main as HTMLElement).findByText(/Your address is confirmed/)
  const sent = calls.filter((c) => c.method === 'POST' && c.path === EMAIL_CONFIRM_API)
  expect(sent).toHaveLength(1)
  expect(sent[0].body).toEqual({ token: 'a-token-for-the-test' })
  expect(window.location.pathname).toBe(EMAIL_CONFIRM_PATH)
})

for (const [label, path] of [
  ['a notification’s link', '/notification/9'],
  ['the email confirmation', `${EMAIL_CONFIRM_PATH}#a-token-for-the-test`],
] as const) {
  test(`signed out, ${label} is the sign-in screen alone, with no frame and no request`, async () => {
    window.history.replaceState(null, '', path)
    const calls = fakeFetch({
      'GET /api/setup': { body: { needed: false } },
      'GET /api/me': { status: 401, body: { error: 'not logged in' } },
    })
    render(<App />)
    await screen.findByRole('button', { name: 'Log in' })
    await waitFor(() => expect(frame().nav).toBeNull())
    expect(calls.some((c) => c.path === EMAIL_CONFIRM_API || c.path.startsWith('/api/me/notifications'))).toBe(false)
  })
}

test('the page’s own links go to the workspace the frame shows, the one used last', async () => {
  localStorage.setItem('taskto.workspace', '4')
  window.history.replaceState(null, '', `${EMAIL_CONFIRM_PATH}#a-token-for-the-test`)
  fakeFetch({ [`POST ${EMAIL_CONFIRM_API}`]: { body: {} } })
  const two = { ...me, workspaces: [...me.workspaces, { id: 4, name: 'Team', role: 'member' }] } as Me
  render(<Shell me={two} onLogout={() => {}} />)
  expect(screen.getByRole('button', { name: 'Workspace: Team' })).toBeTruthy()
  const main = screen.getByRole('main')
  await within(main).findByText(/Your address is confirmed/)
  expect(within(main).getByRole('link', { name: 'Open notification settings' }).getAttribute('href')).toBe(
    '/w/4/settings/notifications',
  )
})

test('the frame stays with its page’s workspace when another tab chooses another one', async () => {
  window.history.replaceState(null, '', '/notification/9')
  fakeFetch({ 'GET /api/me/notifications/9': { status: 404, body: { error: 'not found', code: 'not_found' } } })
  const two = { ...me, workspaces: [...me.workspaces, { id: 4, name: 'Team', role: 'member' }] } as Me
  render(<Shell me={two} onLogout={() => {}} />)
  await screen.findByRole('alert')
  // Another tab uses Team now; this frame is drawn again (its menu opened), its page unchanged.
  localStorage.setItem('taskto.workspace', '4')
  fireEvent.click(screen.getByRole('button', { name: 'Open the menu' }))
  expect(screen.getByRole('button', { name: 'Workspace: Home' })).toBeTruthy()
  expect(
    within(screen.getByRole('main')).getByRole('link', { name: 'Open your notifications' }).getAttribute('href'),
  ).toBe('/w/1/notifications')
})

test('going from one notification’s link to another closes the phone’s menu, and the page is usable', async () => {
  window.history.replaceState(null, '', '/notification/2')
  fakeFetch({
    'GET /api/me/notifications/2': { status: 404, body: { error: 'not found', code: 'not_found' } },
    'GET /api/me/notifications/1': { status: 404, body: { error: 'not found', code: 'not_found' } },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByRole('alert')
  fireEvent.click(screen.getByRole('button', { name: 'Open the menu' }))
  expect(screen.getByRole('main').hasAttribute('inert')).toBe(true)
  act(() => navigate('/notification/1'))
  await waitFor(() => expect(screen.getByRole('main').hasAttribute('inert')).toBe(false))
  expect(screen.getByRole('button', { name: 'Open the menu' })).toBeTruthy()
})
