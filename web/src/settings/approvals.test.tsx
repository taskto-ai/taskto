// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { Me, Workspace } from '../api'
import { type Call, fakeFetch } from '../test/fakes'
import { approval, shop } from '../test/integrations'
import { SettingsPage } from './SettingsPage'

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [],
  can_create_workspace: false,
  is_operator: false,
}
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }
const member: Workspace = { id: 1, name: 'Home', role: 'member' }

const queue = '/api/workspaces/1/integration-approvals'
const waiting = (...items: ReturnType<typeof approval>[]) => ({
  [`GET ${queue}?status=requested`]: { body: { approvals: items, next_cursor: null } },
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const sent = (calls: Call[], method: string, path: string) =>
  calls.filter((c) => c.method === method && c.path === path).map((c) => c.body)

test('the approvals tab is the admin’s: a member sees no tab, and the queue is never asked for', async () => {
  const calls = fakeFetch({ ...waiting(approval()) })
  render(<SettingsPage me={me} workspace={member} tab="integration-approvals" />)
  expect(screen.queryByRole('link', { name: 'Integration approvals' })).toBeNull()
  expect(screen.queryByRole('heading', { name: 'Integration approvals' })).toBeNull()
  expect(calls.filter((c) => c.path.includes('integration-approvals'))).toEqual([])
  cleanup()
  fakeFetch({ ...waiting(approval()) })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  expect(screen.getByRole('link', { name: 'Integration approvals' })).toBeTruthy()
  expect(await screen.findByRole('heading', { name: 'Integration approvals' })).toBeTruthy()
})

test('a request is listed by app, space, requester and date, never by what is in the app', async () => {
  fakeFetch({ ...waiting(approval()) })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  const list = await screen.findByRole('list', { name: 'Requests' })
  const row = within(list).getByRole('listitem')
  expect(within(row).getByText('Invoices')).toBeTruthy()
  expect(row.textContent).toContain('payments · Shop floor · asked by Alice')
  expect(within(row).getByText('Waiting for an admin')).toBeTruthy()
  expect(within(row).getByRole('button', { name: 'Review the request of Invoices' })).toBeTruthy()
})

test('a request in a private space says so without naming the space, and an empty queue says it is empty', async () => {
  fakeFetch({ ...waiting(approval({ space_kind: 'private', space_name: 'Alice' })) })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  const row = within(await screen.findByRole('list', { name: 'Requests' })).getByRole('listitem')
  expect(row.textContent).toContain('Private')
  expect(row.textContent).not.toContain('· Alice ·')
  cleanup()
  fakeFetch({ ...waiting() })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  expect(await screen.findByText('No requests are waiting.')).toBeTruthy()
})

test('the contract is said in plain words before anyone decides', async () => {
  fakeFetch({ ...waiting(approval()), 'GET /api/workspaces/1/integrations': { body: { integrations: [shop] } } })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Review the request of Invoices' }))
  const dialog = await screen.findByRole('dialog', { name: 'Request of Invoices' })
  const text = dialog.textContent ?? ''
  expect(text).toContain('Sends to the payment service the values of “customer”, “number”, and “total”.')
  expect(text).toContain(
    'When the action “send” (it changes a record) runs, it asks for a payment link for the amount in “total”, in EUR',
  )
  expect(text).toContain('The description it sends names “customer”, “number”, and the record’s number.')
  expect(text).toContain('What each of these fields may hold.')
  expect(text).toContain('“total”: number, required, at least 1, at most 100,000.')
  expect(text).toContain('and writes the link to “pay_url” and its number to “payment_id”')
  expect(text).toContain('When a payment succeeds, only if status is “sent”:')
  expect(text).toContain('sets “paid_at” to the time the payment happened')
  expect(text).toContain('sets “status” to “paid”')
  expect(text).toContain('When a payment link expires:')
  expect(text).toContain('Nothing else: no other field, no web address, no key.')
})

test('approving names the integration, and its space is allowed with it when it isn’t yet', async () => {
  const calls = fakeFetch({
    ...waiting(approval({ space_id: 99 })),
    'GET /api/workspaces/1/integrations': {
      body: { integrations: [shop, { ...shop, id: 8, name: 'off', display_name: 'Old shop', disabled: true }] },
    },
    [`POST ${queue}/4/approve`]: { body: approval({ status: 'approved' }) },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Review the request of Invoices' }))
  const dialog = await screen.findByRole('dialog', { name: 'Request of Invoices' })
  const approve = within(dialog).getByRole('button', { name: 'Approve' }) as HTMLButtonElement
  // Nothing is approved until an integration is chosen, and a switched-off one isn't offered. One
  // choice is taken as the answer.
  const select = await within(dialog).findByLabelText('Connect it to')
  expect([...(select as HTMLSelectElement).options].map((o) => o.textContent)).toEqual([
    'Choose an integration',
    'Shop (Test mode)',
  ])
  await waitFor(() => expect(approve.disabled).toBe(false))
  expect(within(dialog).getByText(/Approving also allows it there/)).toBeTruthy()
  // What keeps it allowed afterwards is the integration's own spaces, in the Integrations tab: pointed
  // to, not made again here.
  expect(
    within(dialog)
      .getByRole('link', { name: /Keep … after the approval ends/ })
      .getAttribute('href'),
  ).toContain('integrations')
  fireEvent.click(approve)
  await waitFor(() => expect(sent(calls, 'POST', `${queue}/4/approve`)).toHaveLength(1))
  expect(sent(calls, 'POST', `${queue}/4/approve`)[0]).toEqual({ integration_id: 7, allow_space: true })
  expect(await screen.findByText('Approved Invoices. It can be published now.')).toBeTruthy()
  // The list is asked for again.
  await waitFor(() =>
    expect(calls.filter((c) => c.method === 'GET' && c.path === `${queue}?status=requested`)).toHaveLength(2),
  )
})

test('an integration already allowed in the space is approved without allowing it again', async () => {
  const calls = fakeFetch({
    ...waiting(approval()),
    'GET /api/workspaces/1/integrations': { body: { integrations: [shop] } },
    [`POST ${queue}/4/approve`]: { body: approval({ status: 'approved' }) },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Review the request of Invoices' }))
  const dialog = await screen.findByRole('dialog', { name: 'Request of Invoices' })
  const approve = within(dialog).getByRole('button', { name: 'Approve' }) as HTMLButtonElement
  await waitFor(() => expect(approve.disabled).toBe(false))
  expect(within(dialog).queryByText(/Approving also allows it there/)).toBeNull()
  fireEvent.click(approve)
  await waitFor(() => expect(sent(calls, 'POST', `${queue}/4/approve`)).toHaveLength(1))
  expect(sent(calls, 'POST', `${queue}/4/approve`)[0]).toEqual({ integration_id: 7, allow_space: false })
})

test('with no integration of the kind to connect, approving is not offered and the admin is pointed to Settings', async () => {
  fakeFetch({ ...waiting(approval()), 'GET /api/workspaces/1/integrations': { body: { integrations: [] } } })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Review the request of Invoices' }))
  const dialog = await screen.findByRole('dialog', { name: 'Request of Invoices' })
  expect(await within(dialog).findByText('No integration for Payments is connected and switched on yet.')).toBeTruthy()
  expect((within(dialog).getByRole('button', { name: 'Approve' }) as HTMLButtonElement).disabled).toBe(true)
  expect(within(dialog).getByRole('link', { name: 'Open the integrations' }).getAttribute('href')).toContain(
    'integrations',
  )
})

test('turning a request down sends nothing but the decision', async () => {
  const calls = fakeFetch({
    ...waiting(approval()),
    'GET /api/workspaces/1/integrations': { body: { integrations: [shop] } },
    [`POST ${queue}/4/reject`]: { body: approval({ status: 'rejected' }) },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Review the request of Invoices' }))
  const dialog = await screen.findByRole('dialog', { name: 'Request of Invoices' })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Turn down' }))
  await waitFor(() => expect(sent(calls, 'POST', `${queue}/4/reject`)).toEqual([{}]))
  expect(await screen.findByText('Turned down the request of Invoices.')).toBeTruthy()
})

test('revoking asks first, and says what stops when the contract is the live one’s', async () => {
  const calls = fakeFetch({
    [`GET ${queue}?status=requested`]: { body: { approvals: [], next_cursor: null } },
    [`GET ${queue}?status=approved`]: {
      body: {
        approvals: [
          approval({
            status: 'approved',
            live: true,
            integration: { id: 7, name: 'shop', display_name: 'Shop', usable: true },
            decided_by: { id: 1, name: 'Owner', agent: false },
            decided_at: '2026-11-24T11:00:00.000Z',
          }),
        ],
        next_cursor: null,
      },
    },
    [`POST ${queue}/4/revoke`]: { body: approval({ status: 'revoked' }) },
    'GET /api/workspaces/1/integrations': { body: { integrations: [shop] } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  fireEvent.click(await screen.findByRole('radio', { name: 'Approved' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Review the request of Invoices' }).then((b) => b))
  const dialog = await screen.findByRole('dialog', { name: 'Request of Invoices' })
  expect(within(dialog).getByText('Connected to Shop.')).toBeTruthy()
  expect(within(dialog).getByText('This is what the live release asks.')).toBeTruthy()
  expect(within(dialog).getByText(/Decided by Owner/)).toBeTruthy()
  // An approval can't be approved again, or turned down.
  expect(within(dialog).queryByRole('button', { name: 'Approve' })).toBeNull()
  expect(within(dialog).queryByRole('button', { name: 'Turn down' })).toBeNull()
  fireEvent.click(within(dialog).getByRole('button', { name: 'Revoke approval' }))
  const sure = await screen.findByRole('alertdialog', { name: 'Revoke the approval of Invoices?' })
  expect(within(sure).getByText(/It stops using the integration at once, though it stays online/)).toBeTruthy()
  expect(sent(calls, 'POST', `${queue}/4/revoke`)).toEqual([])
  fireEvent.click(within(sure).getByRole('button', { name: 'Revoke approval' }))
  await waitFor(() => expect(sent(calls, 'POST', `${queue}/4/revoke`)).toEqual([{}]))
  expect(await screen.findByText('Revoked the approval of Invoices.')).toBeTruthy()
})

test('an older page is read with the cursor the server gave', async () => {
  const calls = fakeFetch({
    [`GET ${queue}?status=requested`]: {
      body: { approvals: [approval({ id: 6, app_name: 'Newer' })], next_cursor: 'c/2' },
    },
    [`GET ${queue}?status=requested&cursor=c%2F2`]: {
      body: { approvals: [approval({ id: 5, app_name: 'Older' })], next_cursor: null },
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Show older requests' }))
  expect(await screen.findByText('Older')).toBeTruthy()
  expect(screen.getByText('Newer')).toBeTruthy()
  expect(screen.queryByRole('button', { name: 'Show older requests' })).toBeNull()
  expect(calls.filter((c) => c.path.includes('cursor'))).toHaveLength(1)
})

test('a decision on a request someone decided first closes the dialog, says so and reads the list again', async () => {
  const calls = fakeFetch({
    ...waiting(approval()),
    'GET /api/workspaces/1/integrations': { body: { integrations: [shop] } },
    [`POST ${queue}/4/approve`]: {
      status: 409,
      body: { error: 'x', code: 'app_integration_not_pending' },
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Review the request of Invoices' }))
  const dialog = await screen.findByRole('dialog', { name: 'Request of Invoices' })
  const approve = within(dialog).getByRole('button', { name: 'Approve' }) as HTMLButtonElement
  await waitFor(() => expect(approve.disabled).toBe(false))
  fireEvent.click(approve)
  expect(await screen.findByText('This request was already decided.')).toBeTruthy()
  expect(screen.queryByRole('dialog', { name: 'Request of Invoices' })).toBeNull()
  await waitFor(() =>
    expect(calls.filter((c) => c.method === 'GET' && c.path === `${queue}?status=requested`)).toHaveLength(2),
  )
})

test('a refusal that leaves the request as it was is said in the dialog, which stays open', async () => {
  fakeFetch({
    ...waiting(approval()),
    'GET /api/workspaces/1/integrations': { body: { integrations: [shop] } },
    [`POST ${queue}/4/approve`]: {
      status: 409,
      body: { error: 'x', code: 'app_integration_disabled' },
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Review the request of Invoices' }))
  const dialog = await screen.findByRole('dialog', { name: 'Request of Invoices' })
  const approve = within(dialog).getByRole('button', { name: 'Approve' }) as HTMLButtonElement
  await waitFor(() => expect(approve.disabled).toBe(false))
  fireEvent.click(approve)
  expect(await within(dialog).findByText('This integration is switched off.')).toBeTruthy()
  expect(screen.getByRole('dialog', { name: 'Request of Invoices' })).toBeTruthy()
})

// The approved list, with one approval of the shop in the shared space, and what the integrations say.
function approved(integration: typeof shop) {
  return {
    [`GET ${queue}?status=requested`]: { body: { approvals: [], next_cursor: null } },
    [`GET ${queue}?status=approved`]: {
      body: {
        approvals: [
          approval({
            status: 'approved',
            integration: { id: 7, name: 'shop', display_name: 'Shop', usable: true },
            decided_by: { id: 1, name: 'Owner', agent: false },
            decided_at: '2026-11-24T11:00:00.000Z',
          }),
        ],
        next_cursor: null,
      },
    },
    'GET /api/workspaces/1/integrations': { body: { integrations: [integration] } },
  }
}

async function askToRevoke() {
  fireEvent.click(await screen.findByRole('radio', { name: 'Approved' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Review the request of Invoices' }))
  const dialog = await screen.findByRole('dialog', { name: 'Request of Invoices' })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Revoke approval' }))
  return screen.findByRole('alertdialog', { name: 'Revoke the approval of Invoices?' })
}

test('revoking an approval that allowed the space says the space goes with it, unless it is kept', async () => {
  fakeFetch(approved({ ...shop, spaces: [12], approval_spaces: [12] }))
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  const sure = await askToRevoke()
  expect(within(sure).getByText(/Shop floor also stops being allowed for the integration/)).toBeTruthy()
})

test('revoking an approval whose space an admin allowed says nothing about the space', async () => {
  fakeFetch(approved({ ...shop, spaces: [12], approval_spaces: [] }))
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  const sure = await askToRevoke()
  expect(within(sure).getByText(/It can’t be published, or rolled back to/)).toBeTruthy()
  expect(within(sure).queryByText(/stops being allowed/)).toBeNull()
})

test('a revoke of an approval that is no longer one is said above the list, which is read again', async () => {
  const calls = fakeFetch({
    ...approved({ ...shop, approval_spaces: [] }),
    [`POST ${queue}/4/revoke`]: { status: 409, body: { error: 'x', code: 'app_integration_not_approved' } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  const sure = await askToRevoke()
  fireEvent.click(within(sure).getByRole('button', { name: 'Revoke approval' }))
  expect(await screen.findByText('Only an approval can be revoked.')).toBeTruthy()
  expect(screen.queryByRole('dialog', { name: 'Request of Invoices' })).toBeNull()
  await waitFor(() =>
    expect(calls.filter((c) => c.method === 'GET' && c.path === `${queue}?status=approved`)).toHaveLength(3),
  )
})

test('another filter asks for that status, and a failed list says why', async () => {
  const calls = fakeFetch({
    ...waiting(),
    [`GET ${queue}?status=rejected`]: { status: 403, body: { error: 'x', code: 'forbidden' } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  await screen.findByText('No requests are waiting.')
  fireEvent.click(screen.getByRole('radio', { name: 'Turned down' }))
  await waitFor(() => expect(calls.some((c) => c.path === `${queue}?status=rejected`)).toBe(true))
  expect(await screen.findByRole('alert')).toBeTruthy()
})

test('the revoke warning says what the server holds now, not what the list said when it was read', async () => {
  const stale = approval({
    status: 'approved',
    live: false,
    integration: { id: 7, name: 'shop', display_name: 'Shop', usable: true },
  })
  let reads = 0
  fakeFetch({
    [`GET ${queue}?status=requested`]: { body: { approvals: [], next_cursor: null } },
    [`GET ${queue}?status=approved`]: () => {
      reads++
      return { body: { approvals: [reads === 1 ? stale : { ...stale, live: true }], next_cursor: null } }
    },
    'GET /api/workspaces/1/integrations': { body: { integrations: [shop] } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  fireEvent.click(await screen.findByRole('radio', { name: 'Approved' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Review the request of Invoices' }))
  const dialog = await screen.findByRole('dialog', { name: 'Request of Invoices' })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Revoke approval' }))
  const sure = await screen.findByRole('alertdialog', { name: 'Revoke the approval of Invoices?' })
  expect(within(sure).getByText(/though it stays online/)).toBeTruthy()
})

test('an error of an older page for a filter left behind is not shown over the current queue', async () => {
  let fail: (() => void) | undefined
  fakeFetch({
    [`GET ${queue}?status=requested`]: {
      body: { approvals: [approval({ id: 6, app_name: 'Newer' })], next_cursor: 'c/2' },
    },
    [`GET ${queue}?status=requested&cursor=c%2F2`]: () =>
      new Promise((resolve) => {
        fail = () => resolve({ status: 500, body: { error: 'Old page failed', code: 'internal' } })
      }),
    [`GET ${queue}?status=approved`]: { body: { approvals: [], next_cursor: null } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Show older requests' }))
  fireEvent.click(await screen.findByRole('radio', { name: 'Approved' }))
  await waitFor(() => expect(screen.queryByText('Newer')).toBeNull())
  fail?.()
  await new Promise((r) => setTimeout(r, 20))
  expect(screen.queryByText(/Old page failed|went wrong/i)).toBeNull()
})

test('revoke waits for the integration lookup, and a failed lookup is said and keeps it off', async () => {
  fakeFetch({
    [`GET ${queue}?status=requested`]: { body: { approvals: [], next_cursor: null } },
    [`GET ${queue}?status=approved`]: {
      body: {
        approvals: [
          approval({ status: 'approved', integration: { id: 7, name: 'shop', display_name: 'Shop', usable: true } }),
        ],
        next_cursor: null,
      },
    },
    'GET /api/workspaces/1/integrations': { status: 500, body: { error: 'Lookup failed', code: 'internal' } },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integration-approvals" />)
  fireEvent.click(await screen.findByRole('radio', { name: 'Approved' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Review the request of Invoices' }))
  const dialog = await screen.findByRole('dialog', { name: 'Request of Invoices' })
  expect(await within(dialog).findByRole('alert')).toBeTruthy()
  expect((within(dialog).getByRole('button', { name: 'Revoke approval' }) as HTMLButtonElement).disabled).toBe(true)
})
