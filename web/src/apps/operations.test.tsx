// @vitest-environment jsdom

import { cleanup, render } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import afterAdd from '../../../internal/apps/testdata/expenses.after-add.json'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { fakeFetch } from '../test/fakes'
import { AppStage } from './AppStage'
import { httpTransport } from './AppView'
import { forgetOperations, setOperationUser } from './operations'

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

// Presses the same button as the renderer would, stamping it with the time of the press.
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
  // Answered: the next press is a new one, and so is a different request.
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

  // The press is lost on the first screen.
  await expect(press(httpTransport(base, 3))).rejects.toThrow()
  // The stage draws the app again in place (a refresh) and a transport of its own is made.
  const { rerender, unmount } = render(<AppStage base={base} app={app} reloadKey={0} />)
  rerender(<AppStage base={base} app={app} reloadKey={1} />)
  await expect(press(httpTransport(base, 3))).rejects.toThrow()
  // The screen is left and opened again (a remount), and the answer comes at last.
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
  // A new page: nothing is left in memory but the tab's session storage.
  vi.resetModules()
  const fresh = await import('./operations')
  const { httpTransport: freshTransport } = await import('./AppView')
  fresh.setOperationUser(7)
  down = false
  await press(freshTransport(base, 3))
  expect(operation(calls, 1)).toBe(operation(calls, 0))
  // Another user of this browser never finds it.
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
  [404, { error: 'not found' }], // a proxy's, with no code of the server's
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
  [409, 'app_operation_reused'], // that key can never be used for this request: a new one is made
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
  // The same, with the keys of the context in another order.
  await send(httpTransport(base, 3), { note: 'a', amount: '1' })
  await send(httpTransport(base, 4), { amount: '1', note: 'a' })
  await send(httpTransport(other, 3), { amount: '1', note: 'a' })
  await send(httpTransport(base, 3), { amount: '2', note: 'a' })
  const keys = calls.filter((c) => c.method === 'POST').map((c) => (c.body as { operation: string }).operation)
  expect(keys[1]).toBe(keys[0])
  expect(new Set(keys).size).toBe(4)
})
