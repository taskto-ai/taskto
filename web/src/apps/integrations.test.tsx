// @vitest-environment jsdom
//
// The outside services an app's draft asks for, in the panel before a publish: what each asks in plain
// words, where its approval stands, the request a publisher makes, the gate on publishing, and the
// admin's one step, approve and publish.

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { App, AppDraft, IntegrationStatus, Space } from '../api'
import { setPreferences } from '../i18n'
import { type Call, fakeFetch } from '../test/fakes'
import { shop, slot } from '../test/integrations'
import { EffectItems } from './AppEffects'
import { PublishPanel } from './AppPublish'
import { WorkspaceAdmin } from './admin'

afterEach(async () => {
  cleanup()
  vi.unstubAllGlobals()
  await setPreferences({ user: null })
})

const team: Space = {
  id: 3,
  workspace_id: 1,
  kind: 'shared',
  name: 'Team',
  archived_at: null,
  role: 'editor',
  access: 'write',
}
const mine: Space = { ...team, id: 4, kind: 'private', name: 'Private', role: 'owner' }

const access = {
  roles: ['builder', 'publisher', 'user', 'data_manager', 'reader'],
  restricted: false,
  view: true,
  design: true,
  publish: true,
  submit: true,
  manage_roles: true,
  read_records: 'all',
  modify_records: 'all',
}

const draft: AppDraft = {
  definition_version: 3,
  requires: ['effect.create_payment_link'],
  collection: { fields: [{ name: 'total', type: 'number' }] },
  actions: {
    send: {
      kind: 'update',
      effects: [{ kind: 'create_payment_link', integration: 'payments', amount_field: 'total', currency: 'EUR' }],
    },
  },
  integrations: { payments: { kind: 'payments' } },
} as unknown as AppDraft

function appOf(over: Partial<App> = {}): App {
  return {
    id: 9,
    space_id: 3,
    name: 'Invoices',
    archived_at: null,
    version: 2,
    rows: 0,
    access,
    live: null,
    unpublished_changes: true,
    draft,
    ...over,
  } as unknown as App
}

const status = (...slots: ReturnType<typeof slot>[]): { body: IntegrationStatus } => ({ body: { slots } })

function panel(app: App, space: Space = team, admin = false) {
  const changed = vi.fn()
  render(
    <WorkspaceAdmin value={admin}>
      <PublishPanel base="/api/x" spaceBase="/api/y" app={app} space={space} onChanged={changed} />
    </WorkspaceAdmin>,
  )
  return changed
}

const sent = (calls: Call[], method: string, path: string) =>
  calls.filter((c) => c.method === method && c.path === path).map((c) => c.body)
const adminCalls = (calls: Call[]) => calls.filter((c) => c.path.startsWith('/api/workspaces/1/integration'))

test('the panel says what the app asks of an outside service in words, and where it stands', async () => {
  fakeFetch({ 'GET /api/x/integrations': status(slot()) })
  panel(appOf())
  expect(await screen.findByText('Outside services')).toBeTruthy()
  const section = screen.getByRole('region', { name: 'Service “payments”' })
  expect(within(section).getByText('Not asked for yet')).toBeTruthy()
  expect(section.textContent).toContain('it asks for a payment link for the amount in “total”, in EUR')
  expect(section.textContent).toContain('sets “status” to “paid”')
  // What the contract binds besides the amount: who may trigger the action, the text it sends, and
  // what each field involved may hold. An admin approves all of it, so all of it is said.
  expect(section.textContent).toContain('When the action “send” (it changes a record) runs')
  expect(section.textContent).toContain('The description it sends names “customer”, “number”, and the record’s number.')
  expect(section.textContent).toContain('“total”: number, required, at least 1, at most 100,000.')
  expect(section.textContent).toContain('“status”: choice, required, one of “draft”, “paid”, and “sent”.')
  expect(section.textContent).toContain('“pay_url”: text, may be left empty, up to 2,048 characters.')
  expect(section.textContent).toContain(
    '“receipt”: photos, required when status is “paid”, up to 2 files, each up to 5 MB.',
  )
  // The effect is told with the others, and nothing says the app can reach nothing else.
  expect(screen.getByText('Asks the “payments” outside service for a link to pay the amount in “total”.')).toBeTruthy()
  expect(screen.queryByText(/It can’t reach anything else/)).toBeNull()
})

test('an app with no outside service shows no section and asks the server for none', () => {
  const calls = fakeFetch({})
  const plain = { ...draft, integrations: undefined } as unknown as AppDraft
  panel(appOf({ draft: plain }))
  expect(screen.queryByText('Outside services')).toBeNull()
  expect(calls.filter((c) => c.path.includes('integrations'))).toEqual([])
})

test('a publisher asks for the approval of the contract they read, and publishing waits for it', async () => {
  let asked = false
  const calls = fakeFetch({
    'GET /api/x/integrations': () => status(asked ? slot({ status: 'requested', approval_id: 4 }) : slot()),
    'POST /api/x/integrations/payments/request': () => {
      asked = true
      return { body: { id: 4 } }
    },
  })
  panel(appOf())
  // Until it is approved the app can't be published from here: no publish button, and why.
  expect(
    await screen.findByText('Publishing waits for an admin to approve what this app asks of payments.'),
  ).toBeTruthy()
  expect(screen.queryByRole('button', { name: /publish/i })).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: 'Ask an admin to approve' }))
  await waitFor(() => expect(sent(calls, 'POST', '/api/x/integrations/payments/request')).toHaveLength(1))
  expect(sent(calls, 'POST', '/api/x/integrations/payments/request')[0]).toEqual({ contract_hash: 'a'.repeat(64) })
  expect(await screen.findByText('Waiting for an admin')).toBeTruthy()
  expect(screen.getByText(/An admin will see this request in Settings/)).toBeTruthy()
  expect(screen.queryByRole('button', { name: 'Ask an admin to approve' })).toBeNull()
  // A member never reaches the admin's routes.
  expect(adminCalls(calls)).toEqual([])
})

test('once approved the usual publish is back', async () => {
  fakeFetch({
    'GET /api/x/integrations': status(
      slot({
        status: 'approved',
        approval_id: 4,
        integration: { id: 7, name: 'shop', display_name: 'Shop', usable: true },
      }),
    ),
  })
  panel(appOf())
  expect(await screen.findByText('Connected to Shop.')).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Review and publish…' })).toBeTruthy()
  expect(screen.queryByText(/Publishing waits/)).toBeNull()
})

test('an approval whose integration was switched off holds the publish, and an admin is sent to Settings', async () => {
  fakeFetch({
    'GET /api/x/integrations': status(
      slot({
        status: 'approved',
        approval_id: 4,
        integration: { id: 7, name: 'shop', display_name: 'Shop', usable: false },
      }),
    ),
  })
  panel(appOf(), team, true)
  expect(
    await screen.findByText(/The integration for payments is switched off or isn’t allowed in this space/),
  ).toBeTruthy()
  expect(screen.getByText('Approved, but not usable now')).toBeTruthy()
  expect(screen.queryByRole('button', { name: /publish/i })).toBeNull()
})

test('a turned down or revoked request can be asked for again, and says so', async () => {
  fakeFetch({ 'GET /api/x/integrations': status(slot({ status: 'rejected', approval_id: 4 })) })
  panel(appOf())
  expect(await screen.findByText(/An admin turned this down/)).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Ask again' })).toBeTruthy()
  cleanup()
  fakeFetch({ 'GET /api/x/integrations': status(slot({ status: 'revoked', approval_id: 4 })) })
  panel(appOf())
  expect(await screen.findByText(/An admin withdrew this approval/)).toBeTruthy()
})

test('a contract that changed while it was read is said, and read again', async () => {
  const calls = fakeFetch({
    'GET /api/x/integrations': status(slot()),
    'POST /api/x/integrations/payments/request': {
      status: 409,
      body: { error: 'x', code: 'app_integration_contract_changed' },
    },
  })
  panel(appOf())
  fireEvent.click(await screen.findByRole('button', { name: 'Ask an admin to approve' }))
  expect(await screen.findByText(/The app changed since you read what it asks of the integration/)).toBeTruthy()
  await waitFor(() =>
    expect(calls.filter((c) => c.method === 'GET' && c.path === '/api/x/integrations')).toHaveLength(2),
  )
})

test('someone who can’t publish the app sees where it stands and no way to ask', async () => {
  const calls = fakeFetch({ 'GET /api/x/integrations': status(slot()) })
  panel(appOf({ access: { ...access, publish: false } } as Partial<App>))
  expect(await screen.findByText('Not asked for yet')).toBeTruthy()
  expect(screen.queryByRole('button', { name: 'Ask an admin to approve' })).toBeNull()
  expect(screen.queryByRole('button', { name: /publish/i })).toBeNull()
  expect(adminCalls(calls)).toEqual([])
})

test('live release that can no longer use its service is called out', async () => {
  fakeFetch({
    'GET /api/x/integrations': status(
      slot({
        status: 'approved',
        integration: { id: 7, name: 'shop', display_name: 'Shop', usable: true },
        live: { contract_hash: 'b'.repeat(64), status: 'revoked', usable: false },
      }),
    ),
  })
  panel(appOf({ live: { number: 1 } } as Partial<App>))
  expect(await screen.findByText(/The live release can’t use this service right now/)).toBeTruthy()
})

test('an admin approves and publishes in one step: request, approval, then the publish, in that order', async () => {
  let approved = false
  const calls = fakeFetch({
    'GET /api/x/integrations': () =>
      status(
        approved
          ? slot({ status: 'approved', integration: { id: 7, name: 'shop', display_name: 'Shop', usable: true } })
          : slot(),
      ),
    'GET /api/workspaces/1/integrations': { body: { integrations: [{ ...shop, spaces: [3] }] } },
    'POST /api/x/integrations/payments/request': { body: { id: 4 } },
    'POST /api/workspaces/1/integration-approvals/4/approve': () => {
      approved = true
      return { body: { id: 4 } }
    },
    'POST /api/x/publish': { body: { id: 9, live: { number: 1 } } },
  })
  const changed = panel(appOf(), team, true)
  // The admin has no separate "ask": their step is this one.
  fireEvent.click(await screen.findByRole('button', { name: 'Approve and publish…' }))
  const dialog = await screen.findByRole('dialog', { name: 'Approve and publish' })
  // The contract is in front of them, with their consent said, and who it goes to.
  expect(within(dialog).getByText(/it asks for a payment link for the amount in “total”/)).toBeTruthy()
  expect(
    within(dialog).getByText('By continuing you approve exactly what is listed above, for this app, and publish it.'),
  ).toBeTruthy()
  expect(within(dialog).getByText(/for everyone in Team who may use it/)).toBeTruthy()
  const confirm = within(dialog).getByRole('button', { name: 'Approve and publish' }) as HTMLButtonElement
  await waitFor(() => expect(confirm.disabled).toBe(false))
  expect(within(dialog).queryByText(/also allows the integration/)).toBeNull()
  // Nothing was sent before the button.
  expect(calls.filter((c) => c.method === 'POST')).toEqual([])
  fireEvent.click(confirm)
  await waitFor(() => expect(sent(calls, 'POST', '/api/x/publish')).toHaveLength(1))
  expect(calls.filter((c) => c.method === 'POST').map((c) => c.path)).toEqual([
    '/api/x/integrations/payments/request',
    '/api/workspaces/1/integration-approvals/4/approve',
    '/api/x/publish',
  ])
  expect(sent(calls, 'POST', '/api/x/integrations/payments/request')[0]).toEqual({ contract_hash: 'a'.repeat(64) })
  expect(sent(calls, 'POST', '/api/workspaces/1/integration-approvals/4/approve')[0]).toEqual({
    integration_id: 7,
    allow_space: false,
  })
  expect(sent(calls, 'POST', '/api/x/publish')[0]).toMatchObject({ version: 2 })
  await waitFor(() => expect(changed).toHaveBeenCalled())
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Approve and publish' })).toBeNull())
})

test('a request that already waits is approved as it is, and an integration not yet allowed in the space is allowed with it', async () => {
  const calls = fakeFetch({
    'GET /api/x/integrations': status(slot({ status: 'requested', approval_id: 4 })),
    'GET /api/workspaces/1/integrations': { body: { integrations: [{ ...shop, spaces: [] }] } },
    'POST /api/workspaces/1/integration-approvals/4/approve': { body: { id: 4 } },
    'POST /api/x/publish': { body: { id: 9, live: { number: 1 } } },
  })
  panel(appOf(), mine, true)
  fireEvent.click(await screen.findByRole('button', { name: 'Approve and publish…' }))
  const dialog = await screen.findByRole('dialog', { name: 'Approve and publish' })
  const confirm = within(dialog).getByRole('button', { name: 'Approve and publish' }) as HTMLButtonElement
  await waitFor(() => expect(confirm.disabled).toBe(false))
  expect(within(dialog).getByText(/This also allows the integration in Private/)).toBeTruthy()
  fireEvent.click(confirm)
  await waitFor(() => expect(sent(calls, 'POST', '/api/x/publish')).toHaveLength(1))
  // No second request, and the approval is of the one that waited.
  expect(calls.filter((c) => c.path.endsWith('/request'))).toEqual([])
  expect(sent(calls, 'POST', '/api/workspaces/1/integration-approvals/4/approve')[0]).toEqual({
    integration_id: 7,
    allow_space: true,
  })
})

test('with no integration to connect, an admin can’t approve, and is shown where to connect one', async () => {
  const calls = fakeFetch({
    'GET /api/x/integrations': status(slot()),
    'GET /api/workspaces/1/integrations': { body: { integrations: [{ ...shop, disabled: true }] } },
  })
  panel(appOf(), team, true)
  fireEvent.click(await screen.findByRole('button', { name: 'Approve and publish…' }))
  const dialog = await screen.findByRole('dialog', { name: 'Approve and publish' })
  expect(await within(dialog).findByText('No integration for Payments is connected and switched on yet.')).toBeTruthy()
  expect((within(dialog).getByRole('button', { name: 'Approve and publish' }) as HTMLButtonElement).disabled).toBe(true)
  expect(calls.filter((c) => c.method === 'POST')).toEqual([])
})

test('a refused approval stops the publish and is said in the dialog', async () => {
  const calls = fakeFetch({
    'GET /api/x/integrations': status(slot()),
    'GET /api/workspaces/1/integrations': { body: { integrations: [shop] } },
    'POST /api/x/integrations/payments/request': { body: { id: 4 } },
    'POST /api/workspaces/1/integration-approvals/4/approve': {
      status: 409,
      body: { error: 'x', code: 'app_integration_disabled' },
    },
  })
  panel(appOf(), team, true)
  fireEvent.click(await screen.findByRole('button', { name: 'Approve and publish…' }))
  const dialog = await screen.findByRole('dialog', { name: 'Approve and publish' })
  const confirm = within(dialog).getByRole('button', { name: 'Approve and publish' }) as HTMLButtonElement
  await waitFor(() => expect(confirm.disabled).toBe(false))
  fireEvent.click(confirm)
  expect(await within(dialog).findByText('This integration is switched off.')).toBeTruthy()
  expect(sent(calls, 'POST', '/api/x/publish')).toEqual([])
  // The slots are read again, so a retry starts from what the server holds now.
  await waitFor(() => expect(calls.filter((c) => c.path === '/api/x/integrations')).toHaveLength(2))
})

test('a request asked for again is a new one: the admin approves its id, never the old one’s', async () => {
  const calls = fakeFetch({
    'GET /api/x/integrations': status(slot({ status: 'revoked', approval_id: 4 })),
    'GET /api/workspaces/1/integrations': { body: { integrations: [shop] } },
    'POST /api/x/integrations/payments/request': { body: { id: 11 } },
    'POST /api/workspaces/1/integration-approvals/11/approve': { body: { id: 11 } },
    'POST /api/x/publish': { body: { id: 9, live: { number: 1 } } },
  })
  panel(appOf(), mine, true)
  fireEvent.click(await screen.findByRole('button', { name: 'Approve and publish…' }))
  const dialog = await screen.findByRole('dialog', { name: 'Approve and publish' })
  const confirm = within(dialog).getByRole('button', { name: 'Approve and publish' }) as HTMLButtonElement
  await waitFor(() => expect(confirm.disabled).toBe(false))
  fireEvent.click(confirm)
  await waitFor(() => expect(sent(calls, 'POST', '/api/x/publish')).toHaveLength(1))
  expect(sent(calls, 'POST', '/api/x/integrations/payments/request')).toHaveLength(1)
  expect(adminCalls(calls).filter((c) => c.path.endsWith('/4/approve'))).toEqual([])
  expect(sent(calls, 'POST', '/api/workspaces/1/integration-approvals/11/approve')).toHaveLength(1)
})

test('an admin who may not publish the app is offered no approve and publish', async () => {
  fakeFetch({ 'GET /api/x/integrations': status(slot()) })
  panel(appOf({ access: { ...access, publish: false } } as Partial<App>), team, true)
  expect(await screen.findByText('Not asked for yet')).toBeTruthy()
  expect(screen.queryByRole('button', { name: 'Approve and publish…' })).toBeNull()
})

test('a preview says the payment link it would ask for as a sum, and never as an amount it guessed', () => {
  const effect = (detail: { amount?: number; currency?: string }) =>
    ({
      id: 0,
      operation: '',
      position: 0,
      kind: 'create_payment_link',
      action: 'send',
      record: 1,
      record_revision: 1,
      status: 'simulated',
      attempts: 0,
      next_attempt_at: null,
      error: null,
      detail,
      done_at: null,
      can_retry: false,
    }) as const
  render(<EffectItems effects={[effect({ amount: 120, currency: 'EUR' })]} label="Preview" />)
  const item = within(screen.getByRole('list', { name: 'Preview' })).getByRole('listitem')
  expect(item.textContent).toContain('Create a payment link')
  expect(item.textContent).toContain('Simulated')
  expect(item.textContent).toContain('€120.00')
  cleanup()
  render(<EffectItems effects={[effect({})]} label="Preview" />)
  expect(screen.getByText('Amount from the record')).toBeTruthy()
})

test('publishing stays closed when the status of the outside services can’t be read', async () => {
  fakeFetch({ 'GET /api/x/integrations': { status: 500, body: { error: 'x', code: 'internal' } } })
  panel(appOf(), { ...team, kind: 'private' })
  await waitFor(() => expect(screen.getByRole('alert')).toBeTruthy())
  expect(screen.queryByRole('button', { name: /publish/i })).toBeNull()
})
