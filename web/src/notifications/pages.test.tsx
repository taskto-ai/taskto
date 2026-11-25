// @vitest-environment jsdom

// The two pages a link from outside opens inside the shell: a confirmation email's, and a push's
// /notification/{id}. They are pages like the others: a heading, then what happened in a notice or
// an empty state with the way on.

import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { fakeFetch } from '../test/fakes'
import { EMAIL_CONFIRM_API } from './api'
import { ConfirmEmail } from './ConfirmEmail'
import { OpenNotification } from './NotificationsPage'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

test('a confirmed address is said in a notice under the page heading, with the way to the settings', async () => {
  fakeFetch({ [`POST ${EMAIL_CONFIRM_API}`]: { body: {} } })
  render(<ConfirmEmail token="a-token-for-the-test" workspace={1} />)
  expect(screen.getByRole('heading', { level: 1 })).toBeTruthy()
  expect(screen.getByRole('status').getAttribute('aria-busy')).toBe('true')
  expect((await screen.findByText(/confirmed/i)).closest('[role="status"]')).toBeTruthy()
  expect(screen.getByRole('link', { name: /notification settings/i }).getAttribute('href')).toBe(
    '/w/1/settings/notifications',
  )
})

test('a confirmation that fails says what failed, as an alert', async () => {
  fakeFetch({
    [`POST ${EMAIL_CONFIRM_API}`]: { status: 400, body: { error: 'bad', code: 'invalid_token' } },
  })
  render(<ConfirmEmail token="a-token-for-the-test" workspace={null} />)
  expect(await screen.findByRole('alert')).toBeTruthy()
  expect(screen.queryByRole('link')).toBeNull()
})

test('a notification that is gone says so, with the way to the notifications', async () => {
  fakeFetch({ 'GET /api/me/notifications/9': { status: 404, body: { error: 'not found', code: 'not_found' } } })
  render(<OpenNotification id={9} fallbackWorkspace={1} />)
  expect(screen.getByRole('status')).toBeTruthy()
  expect(await screen.findByRole('alert')).toBeTruthy()
  expect(screen.getByRole('link', { name: 'Open your notifications' }).getAttribute('href')).toBe('/w/1/notifications')
})

test('the notification link page has its heading while it waits and when the notification is gone', async () => {
  fakeFetch({ 'GET /api/me/notifications/9': { status: 404, body: { error: 'not found', code: 'not_found' } } })
  render(<OpenNotification id={9} fallbackWorkspace={1} />)
  expect(screen.getByRole('heading', { level: 1, name: 'Notification' })).toBeTruthy()
  expect(await screen.findByRole('alert')).toBeTruthy()
  expect(screen.getByRole('heading', { level: 1, name: 'Notification' })).toBeTruthy()
})

test('no answer from the server is said as that, in the app’s words, not as a notification that is gone', async () => {
  vi.stubGlobal('fetch', () => Promise.reject(new TypeError('Failed to fetch')))
  render(<OpenNotification id={9} fallbackWorkspace={1} />)
  const alert = await screen.findByRole('alert')
  expect(alert.textContent).toContain("The notification couldn't be opened: the server didn't answer.")
  expect(screen.queryByText(/isn't available any more/)).toBeNull()
  expect(screen.queryByText(/Failed to fetch/)).toBeNull()
  expect(screen.getByRole('button', { name: 'Try again' })).toBeTruthy()
})

test('a confirmation with no answer from the server says so in the app’s words', async () => {
  vi.stubGlobal('fetch', () => Promise.reject(new TypeError('Failed to fetch')))
  render(<ConfirmEmail token="a-token-for-the-test" workspace={1} />)
  const alert = await screen.findByRole('alert')
  expect(alert.textContent).toContain("Your address couldn't be confirmed: the server didn't answer.")
  expect(screen.queryByText(/Failed to fetch/)).toBeNull()
})

test('a failure of the server’s own is not a notification that is gone: it can be tried again', async () => {
  fakeFetch({ 'GET /api/me/notifications/9': { status: 503, body: { error: 'unavailable', code: 'unavailable' } } })
  render(<OpenNotification id={9} fallbackWorkspace={1} />)
  const alert = await screen.findByRole('alert')
  expect(alert.textContent).toContain("The notification couldn't be opened")
  expect(screen.queryByText(/isn't available any more/)).toBeNull()
  expect(screen.getByRole('button', { name: 'Try again' })).toBeTruthy()
})
