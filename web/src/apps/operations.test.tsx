// @vitest-environment jsdom

import { cleanup, render } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import afterAdd from '../../../internal/apps/testdata/expenses.after-add.json'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { fakeFetch } from '../test/fakes'
import { claimDrafts, forgetDrafts } from '../uiState'
import { AppStage } from './AppStage'
import { forgetOperations, setOperationUser } from './operations'
import { httpTransport } from './transport'

const base = '/api/workspaces/1/spaces/2/apps/1'

beforeEach(() => {
  forgetOperations()
  setOperationUser(7)
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  forgetOperations()
  setOperationUser(null)
})

let at = 0
const press = (transport: ReturnType<typeof httpTransport>, context: Record<string, unknown> = { amount: '1' }) =>
  transport.act({
    name: 'add_expense',
    surfaceId: 'app-1',
    sourceComponentId: 'add',
    timestamp: new Date(Date.UTC(2026, 10, 2, 10, 0, at++)).toISOString(),
    context,
  })
const operation = (calls: { body: unknown }[], i: number) => (calls[i].body as { operation: string }).operation

test('an action keeps its operation key until the server has answered it', async () => {
  let fail = true
  const calls = fakeFetch({
    [`POST ${base}/actions`]: () => {
      if (fail) throw new TypeError('network down')
      return { body: afterAdd }
    },
  })
  const transport = httpTransport(base, 3)
  await expect(press(transport)).rejects.toThrow()
  fail = false
  await press(transport)
  expect(operation(calls, 0)).toMatch(/^[A-Za-z0-9_-]{8,64}$/)
  expect(operation(calls, 1)).toBe(operation(calls, 0))
  await press(transport)
  expect(operation(calls, 2)).not.toBe(operation(calls, 0))
})

test('a submission keeps its key when the response is lost, the stage refreshes and the screen is mounted again', async () => {
  let down = true
  const calls = fakeFetch({
    [`GET ${base}/surface?release=3`]: { body: surface },
    [`POST ${base}/actions`]: () => {
      if (down) throw new TypeError('network down')
      return { body: afterAdd }
    },
  })
  const live = { number: 3, definition_version: 1, requires: [] } as never
  const app = { id: 1, version: 1, live, draft: null, access: {}, unpublished_changes: false } as never

  await expect(press(httpTransport(base, 3))).rejects.toThrow()
  const { rerender, unmount } = render(<AppStage base={base} app={app} reloadKey={0} />)
  rerender(<AppStage base={base} app={app} reloadKey={1} />)
  await expect(press(httpTransport(base, 3))).rejects.toThrow()
  unmount()
  render(<AppStage base={base} app={app} reloadKey={0} />)
  down = false
  await press(httpTransport(base, 3))

  const sent = calls.filter((c) => c.method === 'POST')
  expect(sent).toHaveLength(3)
  expect(operation(sent, 1)).toBe(operation(sent, 0))
  expect(operation(sent, 2)).toBe(operation(sent, 0))
})

test('a submission keeps its key across a page reload', async () => {
  let down = true
  const calls = fakeFetch({
    [`POST ${base}/actions`]: () => {
      if (down) throw new TypeError('network down')
      return { body: afterAdd }
    },
  })
  await expect(press(httpTransport(base, 3))).rejects.toThrow()
  vi.resetModules()
  const fresh = await import('./operations')
  const { httpTransport: freshTransport } = await import('./transport')
  fresh.setOperationUser(7)
  down = false
  await press(freshTransport(base, 3))
  expect(operation(calls, 1)).toBe(operation(calls, 0))
  setOperationUser(8)
  await expect(press(httpTransport(base, 3))).resolves.toBeTruthy()
  expect(operation(calls, 2)).not.toBe(operation(calls, 0))
})

test('a gateway timeout leaves the outcome unknown: the retry is the same submission', async () => {
  let status = 504
  const calls = fakeFetch({
    [`POST ${base}/actions`]: () =>
      status === 504 ? { status: 504, body: { error: 'gateway timeout' } } : { body: afterAdd },
  })
  const transport = httpTransport(base, 3)
  await expect(press(transport)).rejects.toThrow()
  status = 200
  await press(transport)
  expect(operation(calls, 1)).toBe(operation(calls, 0))
})

test.each([
  [500, { error: 'x', code: 'internal' }],
  [502, { error: 'bad gateway' }],
  [404, { error: 'not found' }],
  [408, { error: 'x', code: 'request_timeout' }],
  [429, { error: 'x', code: 'rate_limited' }],
])('an answer of %s that doesn’t say the request was refused keeps the key', async (status, body) => {
  const calls = fakeFetch({ [`POST ${base}/actions`]: { status, body } })
  const transport = httpTransport(base, 3)
  await expect(press(transport)).rejects.toThrow()
  await expect(press(transport)).rejects.toThrow()
  expect(operation(calls, 1)).toBe(operation(calls, 0))
})

test.each([
  [422, 'app_field_required'],
  [403, 'app_forbidden'],
  [409, 'app_record_conflict'],
  [409, 'app_release_outdated'],
  [409, 'app_operation_reused'],
])('a %s refused with %s is a definite answer: the next press is a new submission', async (status, code) => {
  const calls = fakeFetch({ [`POST ${base}/actions`]: { status, body: { error: 'x', code } } })
  const transport = httpTransport(base, 3)
  await expect(press(transport)).rejects.toThrow()
  await expect(press(transport)).rejects.toThrow()
  expect(operation(calls, 1)).not.toBe(operation(calls, 0))
})

test('the key is the submission’s: another release, app, action or context has its own', async () => {
  const calls = fakeFetch({ [`POST ${base}/actions`]: () => ({ status: 504, body: { error: 'x' } }) })
  const other = '/api/workspaces/1/spaces/2/apps/2'
  calls.length = 0
  const send = async (transport: ReturnType<typeof httpTransport>, context: Record<string, unknown>) =>
    press(transport, context).catch(() => undefined)
  await send(httpTransport(base, 3), { amount: '1', note: 'a' })
  await send(httpTransport(base, 3), { note: 'a', amount: '1' })
  await send(httpTransport(base, 4), { amount: '1', note: 'a' })
  await send(httpTransport(other, 3), { amount: '1', note: 'a' })
  await send(httpTransport(base, 3), { amount: '2', note: 'a' })
  const keys = calls.filter((c) => c.method === 'POST').map((c) => (c.body as { operation: string }).operation)
  expect(keys[1]).toBe(keys[0])
  expect(new Set(keys).size).toBe(4)
})

test("an answer after the person signed out and another signed in never writes the first one's pending submissions under the second's key", async () => {
  claimDrafts(7)
  let first = true
  let answer: () => void = () => {}
  fakeFetch({
    [`POST ${base}/actions`]: () => {
      if (first) {
        first = false
        return { status: 504, body: null }
      }
      return new Promise((done) => {
        answer = () => done({ body: { messages: afterAdd, rows: [] } })
      })
    },
  })
  const transport = httpTransport(base, 1)
  await expect(press(transport, { amount: '12.50', note: 'private' })).rejects.toBeTruthy()
  expect(sessionStorage.getItem('taskto:operations:7')).toContain('private')
  const held = press(transport, { amount: '3' }).catch(() => {})
  await vi.waitFor(() => expect(answer).not.toBe(null))
  forgetDrafts()
  expect(sessionStorage.getItem('taskto:operations:7')).toBeNull()
  claimDrafts(8)
  sessionStorage.setItem('taskto:operations:8', JSON.stringify([['theirs', 'op-of-theirs-1']]))
  answer()
  await held
  expect(sessionStorage.getItem('taskto:operations:8')).toBe(JSON.stringify([['theirs', 'op-of-theirs-1']]))
  expect(sessionStorage.getItem('taskto:operations:7')).toBeNull()
})

test('an answer after the same person signed out and in again never writes the old keys over the new ones', async () => {
  claimDrafts(7)
  let answer: () => void = () => {}
  fakeFetch({
    [`POST ${base}/actions`]: () =>
      new Promise((done) => {
        answer = () => done({ body: { messages: afterAdd, rows: [] } })
      }),
  })
  const transport = httpTransport(base, 1)
  const held = press(transport, { amount: '3' }).catch(() => {})
  await vi.waitFor(() => expect(sessionStorage.getItem('taskto:operations:7')).not.toBeNull())
  forgetDrafts()
  claimDrafts(7)
  sessionStorage.setItem('taskto:operations:7', JSON.stringify([['new', 'op-of-the-new-sign-in']]))
  answer()
  await held
  expect(sessionStorage.getItem('taskto:operations:7')).toBe(JSON.stringify([['new', 'op-of-the-new-sign-in']]))
})

test("claiming this browser removes another account's pending submissions from this tab, whatever the browser-wide claim says", async () => {
  sessionStorage.setItem('taskto:operations:7', JSON.stringify([['theirs', 'op-of-the-first-person']]))
  sessionStorage.setItem('taskto:operations:8', JSON.stringify([['mine', 'op-of-the-second-person']]))
  localStorage.setItem('taskto:drafts-of', '8')
  claimDrafts(8)
  expect(sessionStorage.getItem('taskto:operations:7')).toBeNull()
  expect(sessionStorage.getItem('taskto:operations:8')).toBe(JSON.stringify([['mine', 'op-of-the-second-person']]))
})
