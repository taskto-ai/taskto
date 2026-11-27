// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { Me } from './api'
import { TimeZonePanel } from './settings/TimeZonePanel'
import { fakeFetch } from './test/fakes'
import { reportTimeZone } from './timezone'

const me = (time_zone?: string): Me => ({
  user: { id: 1, email: 'owner@example.com', name: 'Owner', ...(time_zone === undefined ? {} : { time_zone }) },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
  can_create_workspace: false,
  is_operator: false,
})

function browserIn(zone: string) {
  vi.spyOn(Intl.DateTimeFormat.prototype, 'resolvedOptions').mockReturnValue({
    timeZone: zone,
  } as Intl.ResolvedDateTimeFormatOptions)
}

beforeEach(() => {
  window.history.replaceState(null, '', '/')
  localStorage.clear()
})
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

test("an account with no time zone gets the browser's, once, and the answer is the account", async () => {
  browserIn('America/Sao_Paulo')
  const calls = fakeFetch({
    'PATCH /api/me': () => ({ body: me('America/Sao_Paulo') }),
  })
  const got = await reportTimeZone(me(''))
  expect(got.user.time_zone).toBe('America/Sao_Paulo')
  expect(calls.map((c) => [c.method, c.path, c.body])).toEqual([
    ['PATCH', '/api/me', { browser_time_zone: 'America/Sao_Paulo' }],
  ])
})

test('an account that has a time zone, or a server that has none to give, is left alone', async () => {
  browserIn('Asia/Tokyo')
  const calls = fakeFetch({})
  const set = me('Europe/Lisbon')
  expect(await reportTimeZone(set)).toBe(set)
  const old = me()
  expect(await reportTimeZone(old)).toBe(old)
  expect(calls).toEqual([])
})

test('a browser that does not say its time zone sends nothing, and a refusal never fails the page', async () => {
  browserIn('')
  let calls = fakeFetch({})
  const none = me('')
  expect(await reportTimeZone(none)).toBe(none)
  expect(calls).toEqual([])

  vi.restoreAllMocks()
  browserIn('Asia/Tokyo')
  calls = fakeFetch({ 'PATCH /api/me': { status: 500, body: { error: 'internal error', code: 'internal' } } })
  expect(await reportTimeZone(none)).toBe(none)
  expect(calls).toHaveLength(1)
})

test('the time zone is chosen in settings and saved to the account', async () => {
  browserIn('Europe/Lisbon')
  let zone = 'Asia/Tokyo'
  const calls = fakeFetch({
    'PATCH /api/me': (call) => {
      zone = (call.body as { time_zone: string }).time_zone
      return { body: me(zone) }
    },
  })
  render(<TimeZonePanel initial="Asia/Tokyo" />)
  const select = screen.getByLabelText('Your time zone') as HTMLSelectElement
  expect(select.value).toBe('Asia/Tokyo')
  expect(screen.getByRole('option', { name: 'Not set (UTC)' })).toBeTruthy()

  fireEvent.change(select, { target: { value: 'America/Sao_Paulo' } })
  await screen.findByText('Your time zone is saved.')
  expect(zone).toBe('America/Sao_Paulo')
  expect(select.value).toBe('America/Sao_Paulo')

  fireEvent.click(screen.getByRole('button', { name: "Use this browser's (Europe/Lisbon)" }))
  await waitFor(() => expect(select.value).toBe('Europe/Lisbon'))
  expect(screen.queryByRole('button', { name: /Use this browser's/ })).toBeNull()
  fireEvent.change(select, { target: { value: '' } })
  await waitFor(() => expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(3))
  expect(calls.filter((c) => c.method === 'PATCH').map((c) => c.body)).toEqual([
    { time_zone: 'America/Sao_Paulo' },
    { time_zone: 'Europe/Lisbon' },
    { time_zone: '' },
  ])
})

test('a time zone the server refuses is said so, in the language, and the choice stays as it was', async () => {
  browserIn('Europe/Lisbon')
  fakeFetch({
    'PATCH /api/me': {
      status: 400,
      body: { error: 'time_zone must be …', code: 'time_zone_invalid' },
    },
  })
  render(<TimeZonePanel initial="Asia/Tokyo" />)
  const select = screen.getByLabelText('Your time zone') as HTMLSelectElement
  fireEvent.change(select, { target: { value: 'America/Sao_Paulo' } })
  await screen.findByRole('alert')
  expect(screen.getByRole('alert').textContent).toContain('time zone')
  expect(select.value).toBe('Asia/Tokyo')
  expect(screen.queryByText('Your time zone is saved.')).toBeNull()
})

test("coming back to the page shows the account's zone, not the one the app had at sign-in", async () => {
  browserIn('Europe/Lisbon')
  fakeFetch({ 'GET /api/me': { body: me('Asia/Tokyo') } })
  render(<TimeZonePanel initial="UTC" />)
  const select = screen.getByLabelText('Your time zone') as HTMLSelectElement
  await waitFor(() => expect(select.value).toBe('Asia/Tokyo'))
})

test("a pick that fails to save, made before the account answered, leaves the account's zone shown", async () => {
  browserIn('Europe/Lisbon')
  let answer: () => void = () => {}
  fakeFetch({
    'GET /api/me': () => new Promise((done) => (answer = () => done({ body: me('Asia/Tokyo') }))),
    'PATCH /api/me': { status: 500, body: { error: 'internal error', code: 'internal' } },
  })
  render(<TimeZonePanel initial="UTC" />)
  const select = screen.getByLabelText('Your time zone') as HTMLSelectElement
  fireEvent.change(select, { target: { value: 'America/Sao_Paulo' } })
  await screen.findByRole('alert')
  answer()
  await waitFor(() => expect(select.value).toBe('Asia/Tokyo'))
})

test('a save that fails leaves the last zone the account confirmed, and a late first answer does not replace a saved one', async () => {
  browserIn('Europe/Lisbon')
  let answer: () => void = () => {}
  let saves = 0
  fakeFetch({
    'GET /api/me': () => new Promise((done) => (answer = () => done({ body: me('Asia/Tokyo') }))),
    'PATCH /api/me': (call) =>
      ++saves === 1
        ? { body: me((call.body as { time_zone: string }).time_zone) }
        : { status: 500, body: { error: 'internal error', code: 'internal' } },
  })
  render(<TimeZonePanel initial="UTC" />)
  const select = screen.getByLabelText('Your time zone') as HTMLSelectElement
  fireEvent.change(select, { target: { value: 'Europe/Lisbon' } })
  await waitFor(() => expect(select.value).toBe('Europe/Lisbon'))
  answer()
  await new Promise((r) => setTimeout(r, 20))
  expect(select.value).toBe('Europe/Lisbon')
  fireEvent.change(select, { target: { value: 'America/Sao_Paulo' } })
  await screen.findByRole('alert')
  expect(select.value).toBe('Europe/Lisbon')
})
