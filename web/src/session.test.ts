// @vitest-environment jsdom

// What api() does for the session (session.ts): the account it names, and the 401 it acts on.

import { afterEach, expect, test, vi } from 'vitest'
import { api, SESSION_LOST } from './api'
import { beginSession, endSession, SessionEnded, sameSession, sessionUser, steps } from './session'
import { fakeFetch } from './test/fakes'

afterEach(() => {
  vi.unstubAllGlobals()
})

test('while a session lasts, a request that changes something names its account; a read, or no session, names none', async () => {
  const calls = fakeFetch({
    'GET /api/x': { body: {} },
    'POST /api/x': { body: {} },
    'PATCH /api/x': { body: {} },
    'DELETE /api/x': { status: 204 },
  })
  const named = () => calls.map((c) => c.headers['x-taskto-expected-user'] ?? null)
  await api('/api/x', { method: 'POST', body: {} })
  beginSession(7)
  await api('/api/x')
  await api('/api/x', { method: 'POST', body: {} })
  await api('/api/x', { method: 'PATCH', body: {} })
  await api('/api/x', { method: 'DELETE' })
  // One that names its own (the offline queue, signing out) keeps it.
  await api('/api/x', { method: 'POST', body: {}, headers: { 'X-Taskto-Expected-User': '3' } })
  endSession()
  await api('/api/x', { method: 'POST', body: {} })
  expect(named()).toEqual([null, null, '7', '7', '7', '3', null])
})

test('a 401 ends the session it was sent in, before its caller hears of it', async () => {
  fakeFetch({ 'POST /api/x': { status: 401, body: { error: 'not logged in' } } })
  beginSession(7)
  const same = sameSession()
  const lost = vi.fn()
  window.addEventListener(SESSION_LOST, lost)
  let inCatch: { user: number | null; same: boolean } | null = null
  await api('/api/x', { method: 'POST', body: {} }).catch(() => {
    inCatch = { user: sessionUser(), same: same() }
  })
  window.removeEventListener(SESSION_LOST, lost)
  expect(inCatch).toEqual({ user: null, same: false })
  expect(lost).toHaveBeenCalledTimes(1)
})

test("a 401 to a request of the session before doesn't end the next person's", async () => {
  let answer: () => void = () => {}
  fakeFetch({
    'POST /api/x': () =>
      new Promise((done) => {
        answer = () => done({ status: 401, body: { error: 'not logged in' } })
      }),
  })
  beginSession(7)
  const slow = api('/api/x', { method: 'POST', body: {} }).catch(() => {})
  await vi.waitFor(() => expect(answer).not.toBeNull())
  endSession()
  beginSession(8)
  const lost = vi.fn()
  window.addEventListener(SESSION_LOST, lost)
  answer()
  await slow
  window.removeEventListener(SESSION_LOST, lost)
  expect(lost).not.toHaveBeenCalled()
  expect(sessionUser()).toBe(8)
})

test('steps go on while the session and the screen are there, and stop at the first wait after either goes', () => {
  beginSession(7)
  const alive = { current: true }
  const s = steps(alive)
  expect(s.check('a')).toBe('a')
  alive.current = false
  expect(s.ok()).toBe(false)
  expect(() => s.check('b')).toThrow(SessionEnded)
  const t = steps()
  expect(t.check(1)).toBe(1)
  endSession()
  expect(() => t.check(2)).toThrow(SessionEnded)
})
