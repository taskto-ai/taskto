// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { MailStatus, Me, Workspace } from '../api'
import { setPreferences } from '../i18n'
import { fakeFetch } from '../test/fakes'
import { SettingsPage } from './SettingsPage'

const me: Me = { user: { id: 1, email: 'owner@example.com', name: 'Owner' }, workspaces: [], is_operator: true }
const notOperator: Me = { ...me, is_operator: false }
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }
const member: Workspace = { id: 1, name: 'Home', role: 'member' }

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const configured: MailStatus = {
  configured: true,
  host: 'smtp.example.com',
  port: 587,
  security: 'starttls',
  from: '"TaskTo" <taskto@example.com>',
  password: 'set',
}

test('admins see where the server sends email through, and send a test to their own address', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/mail': { body: configured },
    'POST /api/workspaces/1/mail/test': { body: { sent_to: 'owner@example.com' } },
  })
  render(<SettingsPage me={me} workspace={admin} />)
  expect(await screen.findByText('smtp.example.com, port 587')).toBeTruthy()
  expect(screen.getByText('STARTTLS (upgraded to TLS)')).toBeTruthy()
  expect(screen.getByText('With a user name and password')).toBeTruthy()
  expect(screen.getByText(/The test goes to your own address, owner@example.com/)).toBeTruthy()

  fireEvent.click(screen.getByRole('button', { name: 'Send a test email' }))
  expect((await screen.findByRole('status')).textContent).toMatch(/Test email sent to owner@example.com/)
  // The request names no recipient: the server sends to the caller.
  expect(calls.filter((c) => c.method === 'POST').map((c) => [c.path, c.body])).toEqual([
    ['/api/workspaces/1/mail/test', {}],
  ])
})

test('a failed test says at which step, in the admin language, never the server text', async () => {
  fakeFetch({
    'GET /api/workspaces/1/mail': { body: configured },
    'POST /api/workspaces/1/mail/test': {
      status: 502,
      body: { error: 'the SMTP server refused the sign-in', code: 'mail_auth_failed' },
    },
  })
  render(<SettingsPage me={me} workspace={admin} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Send a test email' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(
    /The SMTP server refused the sign-in\. Check the user name/,
  )
  expect(screen.queryByRole('status')).toBeNull()

  cleanup()
  fakeFetch({ 'GET /api/workspaces/1/mail': { body: configured } })
  await act(() => setPreferences({ user: 'pt-BR' }))
  render(<SettingsPage me={me} workspace={admin} />)
  expect(await screen.findByText('Enviar e-mail de teste')).toBeTruthy()
  expect(screen.getByText('smtp.example.com, porta 587')).toBeTruthy()
  await act(() => setPreferences({ user: null }))
})

test('a server without an SMTP server says so, and offers no test', async () => {
  fakeFetch({ 'GET /api/workspaces/1/mail': { body: { configured: false } } })
  render(<SettingsPage me={me} workspace={admin} />)
  expect(await screen.findByText(/This server can't send email yet/)).toBeTruthy()
  expect(screen.queryByRole('button', { name: 'Send a test email' })).toBeNull()
})

test("members don't see the email settings, and don't ask for them", async () => {
  const calls = fakeFetch({})
  render(<SettingsPage me={me} workspace={member} tab="about" />)
  await screen.findByText(/Nothing this workspace uses now asks for a credit/)
  expect(calls.filter((c) => c.path.includes('/mail'))).toEqual([])
})

test("an admin who doesn't run the server doesn't see the email settings or ask for them", async () => {
  const calls = fakeFetch({})
  render(<SettingsPage me={notOperator} workspace={admin} />)
  await screen.findByRole('heading', { name: 'General' })
  expect(screen.queryByRole('heading', { name: 'Email' })).toBeNull()
  expect(calls.filter((c) => c.path.includes('/mail'))).toEqual([])
})
