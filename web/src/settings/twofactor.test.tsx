// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { Me, Workspace } from '../api'
import { type Call, fakeFetch } from '../test/fakes'
import { SettingsPage } from './SettingsPage'

// The QR writer is a .wasm file the browser loads: the panel is tested with a drawing in its place,
// and the URI it is asked to draw.
const drawn = vi.hoisted(() => ({ texts: [] as string[] }))
vi.mock('./qr', () => ({
  qrSvg: async (text: string) => {
    drawn.texts.push(text)
    return '<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"/>'
  },
  svgSource: (svg: string) => `data:image/svg+xml,${encodeURIComponent(svg)}`,
}))

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
}
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }
const member: Workspace = { id: 1, name: 'Home', role: 'member' }

const sent = (calls: Call[], method: string, path: string) =>
  calls.filter((c) => c.method === method && c.path === path).map((c) => c.body ?? null)

const URI = 'otpauth://totp/TaskTo:owner@example.com?secret=AAAABBBBCCCCDDDD&issuer=TaskTo'

beforeEach(() => {
  window.history.replaceState(null, '', '/')
  drawn.texts.length = 0
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

test('turning it on: the password, then the secret as a QR code and as text, then a code, then the recovery codes once', async () => {
  let enabled = false
  const calls = fakeFetch({
    'GET /api/me/two-factor': () => ({ body: { enabled, recovery_codes_left: enabled ? 10 : 0 } }),
    'POST /api/me/two-factor/enrol': (call) =>
      (call.body as { password: string }).password === 'fake-password-123'
        ? { body: { secret: 'AAAABBBBCCCCDDDD', otpauth_uri: URI } }
        : { status: 403, body: { error: 'wrong', code: 'wrong_password' } },
    'POST /api/me/two-factor/confirm': (call) => {
      if ((call.body as { code: string }).code !== '123456')
        return { status: 403, body: { error: 'wrong', code: 'wrong_two_factor_code' } }
      enabled = true
      return { body: { recovery_codes: ['aaaa-bbbb-cccc', 'dddd-eeee-ffff'] } }
    },
  })
  render(<SettingsPage me={me} workspace={member} tab="password" />)
  await screen.findByText('Two-factor login is off.')

  fireEvent.change(screen.getByLabelText('Your password'), { target: { value: 'fake-wrong-password' } })
  fireEvent.click(screen.getByRole('button', { name: 'Turn on two-factor login' }))
  expect((await screen.findAllByRole('alert')).map((a) => a.textContent).join()).toContain('current password is wrong')
  expect(drawn.texts).toEqual([])

  fireEvent.change(screen.getByLabelText('Your password'), { target: { value: 'fake-password-123' } })
  fireEvent.click(screen.getByRole('button', { name: 'Turn on two-factor login' }))
  const secret = (await screen.findByLabelText('Secret for the authenticator app')) as HTMLInputElement
  expect(secret.value).toBe('AAAABBBBCCCCDDDD')
  const qr = await screen.findByRole('img', { name: 'QR code for your authenticator app' })
  expect((qr as HTMLImageElement).src).toContain('data:image/svg+xml')
  expect(drawn.texts).toEqual([URI]) // drawn here, from what the server sent: nothing goes elsewhere to be drawn
  // Nothing is on until a right code: the status is still off.
  expect(sent(calls, 'GET', '/api/me/two-factor')).toHaveLength(1)

  const code = screen.getByLabelText('Then enter the six-digit code the app shows')
  fireEvent.change(code, { target: { value: '000000' } })
  fireEvent.click(screen.getByRole('button', { name: 'Confirm and turn on' }))
  expect((await screen.findAllByRole('alert'))[0].textContent).toContain('code is wrong')

  fireEvent.change(code, { target: { value: '123456' } })
  fireEvent.click(screen.getByRole('button', { name: 'Confirm and turn on' }))
  const list = await screen.findByRole('list', { name: 'Your recovery codes' })
  expect(list.textContent).toBe('aaaa-bbbb-ccccdddd-eeee-ffff')
  expect(sent(calls, 'POST', '/api/me/two-factor/confirm').at(-1)).toEqual({ code: '123456' })

  // Once they are saved the codes are gone from the page, and the panel says it is on.
  fireEvent.click(screen.getByRole('button', { name: 'I saved them' }))
  await screen.findByText('Two-factor login is on.')
  expect(screen.queryByText('aaaa-bbbb-cccc')).toBeNull()
  expect(screen.getByText('10 recovery codes left.')).toBeTruthy()
})

test('with it on: new recovery codes and turning it off take the password and a code', async () => {
  let enabled = true
  const calls = fakeFetch({
    'GET /api/me/two-factor': () => ({ body: { enabled, recovery_codes_left: 3 } }),
    'POST /api/me/two-factor/recovery-codes': { body: { recovery_codes: ['1111-2222-3333'] } },
    'POST /api/me/two-factor/disable': (call) => {
      if ((call.body as { code: string }).code !== '654321')
        return { status: 403, body: { error: 'wrong', code: 'wrong_two_factor_code' } }
      enabled = false
      return { status: 204 }
    },
  })
  render(<SettingsPage me={me} workspace={member} tab="password" />)
  await screen.findByText('Two-factor login is on.')
  expect(screen.getByText('3 recovery codes left.')).toBeTruthy()
  // Both actions wait for the password and a code.
  const newCodes = screen.getByRole('button', { name: 'Make new recovery codes' }) as HTMLButtonElement
  expect(newCodes.disabled).toBe(true)
  expect((screen.getByRole('button', { name: 'Turn off…' }) as HTMLButtonElement).disabled).toBe(true)

  fireEvent.change(screen.getByLabelText('Your password'), { target: { value: 'fake-password-123' } })
  fireEvent.change(screen.getByLabelText('Code from your app, or a recovery code'), { target: { value: '654321' } })
  fireEvent.click(newCodes)
  await screen.findByRole('list', { name: 'Your recovery codes' })
  expect(sent(calls, 'POST', '/api/me/two-factor/recovery-codes')).toEqual([
    { password: 'fake-password-123', code: '654321' },
  ])
  fireEvent.click(screen.getByRole('button', { name: 'I saved them' }))
  await screen.findByText('Two-factor login is on.')

  // The form was emptied after the first use. A wrong code, then the right one; it asks before it turns off.
  fireEvent.change(screen.getByLabelText('Your password'), { target: { value: 'fake-password-123' } })
  fireEvent.change(screen.getByLabelText('Code from your app, or a recovery code'), { target: { value: '000000' } })
  fireEvent.click(screen.getByRole('button', { name: 'Turn off…' }))
  expect(sent(calls, 'POST', '/api/me/two-factor/disable')).toEqual([])
  fireEvent.click(screen.getByRole('button', { name: 'Yes, turn off' }))
  expect((await screen.findByRole('alert')).textContent).toContain('code is wrong')
  fireEvent.change(screen.getByLabelText('Code from your app, or a recovery code'), { target: { value: '654321' } })
  fireEvent.click(screen.getByRole('button', { name: 'Yes, turn off' }))
  await screen.findByText('Two-factor login is off.')
  expect(sent(calls, 'POST', '/api/me/two-factor/disable').at(-1)).toEqual({
    password: 'fake-password-123',
    code: '654321',
  })
})

test('cancelling the enrolment forgets the secret', async () => {
  fakeFetch({
    'GET /api/me/two-factor': { body: { enabled: false, recovery_codes_left: 0 } },
    'POST /api/me/two-factor/enrol': { body: { secret: 'AAAABBBBCCCCDDDD', otpauth_uri: URI } },
  })
  render(<SettingsPage me={me} workspace={member} tab="password" />)
  await screen.findByText('Two-factor login is off.')
  fireEvent.change(screen.getByLabelText('Your password'), { target: { value: 'fake-password-123' } })
  fireEvent.click(screen.getByRole('button', { name: 'Turn on two-factor login' }))
  await screen.findByLabelText('Secret for the authenticator app')
  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
  await screen.findByText('Two-factor login is off.')
  expect(screen.queryByLabelText('Secret for the authenticator app')).toBeNull()
})

const people = [
  {
    id: 1,
    email: 'owner@example.com',
    name: 'Owner',
    role: 'admin',
    joined_at: '2026-11-01T00:00:00.000Z',
    two_factor: true,
  },
  {
    id: 2,
    email: 'alice@example.com',
    name: 'Alice',
    role: 'member',
    joined_at: '2026-11-02T00:00:00.000Z',
    two_factor: true,
  },
  {
    id: 3,
    email: 'bob@example.com',
    name: 'Bob',
    role: 'member',
    joined_at: '2026-11-03T00:00:00.000Z',
    two_factor: false,
  },
]

test("an admin clears a member's two-factor login, once asked, only where the member has it on", async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/members': { body: { members: people } },
    'GET /api/workspaces/1/invites': { body: { invites: [] } },
    'DELETE /api/workspaces/1/members/2/two-factor': { status: 204 },
  })
  render(<SettingsPage me={me} workspace={admin} tab="members" />)
  await screen.findByText('alice@example.com')
  // Not for yourself (Settings has it), and not for a member who has none.
  expect(screen.queryByRole('button', { name: 'Clear two-factor Owner' })).toBeNull()
  expect(screen.queryByRole('button', { name: 'Clear two-factor Bob' })).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: 'Clear two-factor Alice' }))
  expect(sent(calls, 'DELETE', '/api/workspaces/1/members/2/two-factor')).toEqual([])
  fireEvent.click(screen.getByRole('button', { name: 'Yes, clear it' }))
  await waitFor(() => expect(sent(calls, 'DELETE', '/api/workspaces/1/members/2/two-factor')).toHaveLength(1))
})

test('a refused clear says why', async () => {
  fakeFetch({
    'GET /api/workspaces/1/members': { body: { members: people } },
    'GET /api/workspaces/1/invites': { body: { invites: [] } },
    'DELETE /api/workspaces/1/members/2/two-factor': {
      status: 403,
      body: { error: 'elsewhere', code: 'two_factor_clear_elsewhere' },
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="members" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Clear two-factor Alice' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, clear it' }))
  expect((await screen.findByRole('alert')).textContent).toContain("you don't administer")
})
