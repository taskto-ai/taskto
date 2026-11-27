// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { MailStatus, Me, Workspace } from '../api'
import { type Call, fakeFetch } from '../test/fakes'
import { SettingsPage } from './SettingsPage'

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
  can_create_workspace: true,
  is_operator: true,
}
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const mail = (sendsLinks: boolean): MailStatus => ({
  configured: true,
  sends_links: sendsLinks,
  host: 'smtp.example.com',
  port: 587,
  security: 'starttls',
  from: 'taskto@example.com',
  password: 'set',
})

const made = (extra: Record<string, unknown>) => ({
  status: 201,
  body: {
    id: 5,
    email: 'ana@example.com',
    role: 'member',
    created_at: '2026-11-18T00:00:00.000Z',
    expires_at: '2026-11-25T00:00:00.000Z',
    token: 'fake-invite-token',
    ...extra,
  },
})

const invitePosts = (calls: Call[]) => calls.filter((c) => c.method === 'POST' && c.path.endsWith('/invites'))

function routes(status: MailStatus | null, reply: ReturnType<typeof made>) {
  return {
    'GET /api/workspaces/1/members': { body: { members: [] } },
    'GET /api/workspaces/1/invites': { body: { invites: [] } },
    'GET /api/login/methods': { body: { password: true, sso: [] } },
    ...(status ? { 'GET /api/workspaces/1/mail': { body: status } } : {}),
    'POST /api/workspaces/1/invites': reply,
  }
}

async function invite() {
  fireEvent.change(await screen.findByLabelText('Email'), { target: { value: 'ana@example.com' } })
  fireEvent.click(screen.getByRole('button', { name: 'Invite' }))
}

test('where the server emails links, the invite can be emailed too, and the link is still there to copy', async () => {
  const calls = fakeFetch(routes(mail(true), made({ email_sent: true })))
  render(<SettingsPage me={me} workspace={admin} tab="members" />)
  const box = (await screen.findByLabelText('Email the invite link to this address')) as HTMLInputElement
  expect(box.checked).toBe(true)
  await invite()
  const notice = await screen.findByRole('status')
  expect(notice.textContent).toMatch(/The invite was emailed to ana@example.com/)
  expect((screen.getByLabelText('Invite link') as HTMLInputElement).value).toMatch(/\/invite#fake-invite-token$/)
  expect(invitePosts(calls).map((c) => c.body)).toEqual([
    { email: 'ana@example.com', role: 'member', send_email: true },
  ])
})

test('the email can be left out, and the invite is made as before', async () => {
  const calls = fakeFetch(routes(mail(true), made({})))
  render(<SettingsPage me={me} workspace={admin} tab="members" />)
  fireEvent.click(await screen.findByLabelText('Email the invite link to this address'))
  await invite()
  expect((await screen.findByRole('status')).textContent).toMatch(/Send this link to ana@example.com/)
  expect(invitePosts(calls).map((c) => c.body)).toEqual([{ email: 'ana@example.com', role: 'member' }])
})

test('a failed email says so and leaves the link to send by hand', async () => {
  fakeFetch(routes(mail(true), made({ email_sent: false })))
  render(<SettingsPage me={me} workspace={admin} tab="members" />)
  await screen.findByLabelText('Email the invite link to this address')
  await invite()
  expect((await screen.findByRole('status')).textContent).toMatch(/couldn't be sent.*Send them this link yourself/)
  expect((screen.getByLabelText('Invite link') as HTMLInputElement).value).toMatch(/#fake-invite-token$/)
})

test('without email on the server, or when it cannot be known, the dialog is as it was', async () => {
  for (const status of [{ configured: false } as MailStatus, mail(false), null]) {
    cleanup()
    const calls = fakeFetch(routes(status, made({})))
    render(<SettingsPage me={me} workspace={admin} tab="members" />)
    await screen.findByLabelText('Email')
    await waitFor(() => expect(calls.some((c) => c.path === '/api/workspaces/1/mail')).toBe(true))
    expect(screen.queryByLabelText('Email the invite link to this address')).toBeNull()
    await invite()
    expect((await screen.findByRole('status')).textContent).toMatch(/Send this link to ana@example.com/)
    expect(invitePosts(calls).map((c) => c.body)).toEqual([{ email: 'ana@example.com', role: 'member' }])
  }
})

test('a server with email but no public address says why links are not emailed', async () => {
  fakeFetch({ 'GET /api/workspaces/1/mail': { body: mail(false) } })
  render(<SettingsPage me={me} workspace={admin} />)
  expect(await screen.findByText(/doesn't email reset or invite links yet/)).toBeTruthy()
})

test("the invite form says the server doesn't email invites only when it read the server's email status", async () => {
  for (const status of [{ configured: false } as MailStatus, mail(false)]) {
    cleanup()
    fakeFetch(routes(status, made({})))
    render(<SettingsPage me={me} workspace={admin} tab="members" />)
    expect(await screen.findByText(/This server doesn't email invites/)).toBeTruthy()
  }
  for (const refusal of [
    { status: 403, body: { error: 'operators only' } },
    { status: 500, body: {} },
  ]) {
    cleanup()
    const calls = fakeFetch({ ...routes(null, made({})), 'GET /api/workspaces/1/mail': refusal })
    render(<SettingsPage me={me} workspace={admin} tab="members" />)
    await screen.findByLabelText('Email')
    await waitFor(() => expect(calls.some((c) => c.path === '/api/workspaces/1/mail')).toBe(true))
    await act(() => new Promise((resolve) => setTimeout(resolve, 20)))
    expect(screen.queryByText(/This server doesn't email invites/)).toBeNull()
  }
  cleanup()
  fakeFetch(routes(mail(true), made({})))
  render(<SettingsPage me={me} workspace={admin} tab="members" />)
  await screen.findByLabelText('Email the invite link to this address')
  expect(screen.queryByText(/This server doesn't email invites/)).toBeNull()
})
