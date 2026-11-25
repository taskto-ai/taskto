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
