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
      effects: [
        {
          kind: 'create_payment_link',
          integration: 'payments',
          amount_field: 'total',
          currency: 'EUR',
          url_field: 'pay_url',
          id_field: 'payment_id',
        },
      ],
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
  expect(
    screen.getByText(
      'Asks the “payments” integration for a link to pay total in EUR, and puts the link in pay_url and its id in payment_id.',
    ),
  ).toBeTruthy()
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

// ----- the admin's step when something goes wrong part of the way -----

const approvedShop = { id: 7, name: 'shop', display_name: 'Shop', usable: true }
const billing = () => slot({ slot: 'billing', contract_hash: 'b'.repeat(64) })

async function openApproveAndPublish() {
  fireEvent.click(await screen.findByRole('button', { name: 'Approve and publish…' }))
  const dialog = await screen.findByRole('dialog', { name: 'Approve and publish' })
  const confirm = within(dialog).getByRole('button', { name: 'Approve and publish' }) as HTMLButtonElement
  await waitFor(() => expect(confirm.disabled).toBe(false))
  return { dialog, confirm }
}

test('a contract that changed under the admin closes the dialog, reads the app and the slots again and says the draft changed', async () => {
  let first = false
  const calls = fakeFetch({
    'GET /api/x/integrations': () =>
      status(first ? slot({ status: 'approved', integration: approvedShop }) : slot(), billing()),
    'GET /api/workspaces/1/integrations': { body: { integrations: [{ ...shop, spaces: [3] }] } },
    'POST /api/x/integrations/payments/request': { body: { id: 4 } },
    'POST /api/workspaces/1/integration-approvals/4/approve': () => {
      first = true
      return { body: { id: 4 } }
    },
    'POST /api/x/integrations/billing/request': {
      status: 409,
      body: { error: 'x', code: 'app_integration_contract_changed' },
    },
  })
  const changed = panel(appOf(), team, true)
  const { confirm } = await openApproveAndPublish()
  const slotReads = () => calls.filter((c) => c.method === 'GET' && c.path === '/api/x/integrations').length
  const before = slotReads()
  fireEvent.click(confirm)
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Approve and publish' })).toBeNull())
  expect(changed).toHaveBeenCalled()
  await waitFor(() => expect(slotReads()).toBeGreaterThan(before))
  expect(screen.getByText(/The draft changed while you were reviewing it/)).toBeTruthy()
  // The first slot was approved before the second was refused: that stands, and is said.
  expect(await screen.findByText(/These approvals went through and stay active: payments\./)).toBeTruthy()
  expect(sent(calls, 'POST', '/api/x/publish')).toEqual([])
})

test('approvals that stand after a contract change are said even when the panel published before', async () => {
  let phase: 'first' | 'second' = 'first'
  let firstDone = false
  fakeFetch({
    'GET /api/x/integrations': () =>
      phase === 'first'
        ? status(slot())
        : status(firstDone ? slot({ status: 'approved', integration: approvedShop }) : slot(), billing()),
    'GET /api/workspaces/1/integrations': { body: { integrations: [{ ...shop, spaces: [3] }] } },
    'POST /api/x/integrations/payments/request': { body: { id: 4 } },
    'POST /api/x/integrations/billing/request': {
      status: 409,
      body: { error: 'x', code: 'app_integration_contract_changed' },
    },
    'POST /api/workspaces/1/integration-approvals/4/approve': () => {
      if (phase === 'second') firstDone = true
      return { body: { id: 4 } }
    },
    'POST /api/x/publish': { body: { id: 9, live: { number: 1 } } },
  })
  const element = (app: App) => (
    <WorkspaceAdmin value={true}>
      <PublishPanel base="/api/x" spaceBase="/api/y" app={app} space={team} onChanged={() => {}} />
    </WorkspaceAdmin>
  )
  const view = render(element(appOf()))
  const first = await openApproveAndPublish()
  fireEvent.click(first.confirm)
  expect(await screen.findByText(/Published as release 1|release 1/)).toBeTruthy()
  // The draft changes, asks for a second service, and the admin starts again: the second is refused.
  phase = 'second'
  view.rerender(element(appOf({ version: 3 })))
  const second = await openApproveAndPublish()
  fireEvent.click(second.confirm)
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Approve and publish' })).toBeNull())
  expect(await screen.findByText(/These approvals went through and stay active: payments\./)).toBeTruthy()
})

test('the retained approvals stay said when the admin opens another attempt', async () => {
  let firstDone = false
  fakeFetch({
    'GET /api/x/integrations': () =>
      status(firstDone ? slot({ status: 'approved', integration: approvedShop }) : slot(), billing()),
    'GET /api/workspaces/1/integrations': { body: { integrations: [{ ...shop, spaces: [3] }] } },
    'POST /api/x/integrations/payments/request': { body: { id: 4 } },
    'POST /api/x/integrations/billing/request': {
      status: 409,
      body: { error: 'x', code: 'app_integration_contract_changed' },
    },
    'POST /api/workspaces/1/integration-approvals/4/approve': () => {
      firstDone = true
      return { body: { id: 4 } }
    },
  })
  panel(appOf(), team, true)
  const { confirm } = await openApproveAndPublish()
  fireEvent.click(confirm)
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Approve and publish' })).toBeNull())
  expect(await screen.findByText(/These approvals went through and stay active: payments\./)).toBeTruthy()
  fireEvent.click(await screen.findByRole('button', { name: 'Approve and publish…' }))
  await screen.findByRole('dialog', { name: 'Approve and publish' })
  expect(screen.getByText(/These approvals went through and stay active: payments\./)).toBeTruthy()
})

test('an approval an admin has revoked since is not said to stay active', async () => {
  let firstDone = false
  fakeFetch({
    'GET /api/x/integrations': () =>
      status(firstDone ? slot({ status: 'revoked', approval_id: 4 }) : slot(), billing()),
    'GET /api/workspaces/1/integrations': { body: { integrations: [{ ...shop, spaces: [3] }] } },
    'POST /api/x/integrations/payments/request': { body: { id: 4 } },
    'POST /api/x/integrations/billing/request': {
      status: 409,
      body: { error: 'x', code: 'app_integration_contract_changed' },
    },
    'POST /api/workspaces/1/integration-approvals/4/approve': () => {
      firstDone = true
      return { body: { id: 4 } }
    },
  })
  panel(appOf(), team, true)
  const { confirm } = await openApproveAndPublish()
  fireEvent.click(confirm)
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Approve and publish' })).toBeNull())
  await screen.findByText(/The draft changed while you were reviewing it/)
  expect(screen.queryByText(/These approvals went through/)).toBeNull()
})

test('a refusal that arrives after the dialog was closed does not close the one opened since', async () => {
  let requests = 0
  let refuse: (() => void) | undefined
  fakeFetch({
    'GET /api/x/integrations': status(slot()),
    'GET /api/workspaces/1/integrations': { body: { integrations: [{ ...shop, spaces: [3] }] } },
    'POST /api/x/integrations/payments/request': () => {
      requests++
      if (requests > 1) return { body: { id: 4 } }
      return new Promise((resolve) => {
        refuse = () => resolve({ status: 409, body: { error: 'x', code: 'app_integration_contract_changed' } })
      })
    },
  })
  const element = (app: App) => (
    <WorkspaceAdmin value={true}>
      <PublishPanel base="/api/x" spaceBase="/api/y" app={app} space={team} onChanged={() => {}} />
    </WorkspaceAdmin>
  )
  const view = render(element(appOf()))
  const { confirm } = await openApproveAndPublish()
  fireEvent.click(confirm)
  await waitFor(() => expect(refuse).toBeDefined())
  // The draft changes under the first attempt, which closes; the admin opens a new dialog.
  view.rerender(element(appOf({ version: 3 })))
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Approve and publish' })).toBeNull())
  await openApproveAndPublish()
  // Now the first attempt's refusal arrives: the new dialog stays.
  refuse?.()
  await new Promise((r) => setTimeout(r, 30))
  expect(screen.getByRole('dialog', { name: 'Approve and publish' })).toBeTruthy()
})

test('an approval made by an attempt the draft superseded is still said to stay active, whatever came after it', async () => {
  let firstDone = false
  let release: (() => void) | undefined
  fakeFetch({
    'GET /api/x/integrations': () =>
      status(firstDone ? slot({ status: 'approved', integration: approvedShop }) : slot(), billing()),
    'GET /api/workspaces/1/integrations': { body: { integrations: [{ ...shop, spaces: [3] }] } },
    'POST /api/x/integrations/payments/request': { body: { id: 4 } },
    'POST /api/workspaces/1/integration-approvals/4/approve': () =>
      new Promise((resolve) => {
        release = () => {
          firstDone = true
          resolve({ body: { id: 4 } })
        }
      }),
    'POST /api/x/integrations/billing/request': {
      status: 409,
      body: { error: 'x', code: 'app_integration_contract_changed' },
    },
  })
  const element = (app: App) => (
    <WorkspaceAdmin value={true}>
      <PublishPanel base="/api/x" spaceBase="/api/y" app={app} space={team} onChanged={() => {}} />
    </WorkspaceAdmin>
  )
  const view = render(element(appOf()))
  const { confirm } = await openApproveAndPublish()
  fireEvent.click(confirm)
  await waitFor(() => expect(release).toBeDefined())
  // The draft changes while the first approval is in flight: the dialog closes and says so.
  view.rerender(element(appOf({ version: 3 })))
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Approve and publish' })).toBeNull())
  expect(screen.queryByText(/These approvals went through/)).toBeNull()
  // The approval goes through after that, and the second request is refused: the admin is still told.
  release?.()
  expect(await screen.findByText(/These approvals went through and stay active: payments\./)).toBeTruthy()
})

test('a slot the draft no longer declares closes the dialog, reads the app and the slots again and says the draft changed', async () => {
  let reads = 0
  const calls = fakeFetch({
    'GET /api/x/integrations': () => {
      reads++
      return status(slot())
    },
    'GET /api/workspaces/1/integrations': { body: { integrations: [{ ...shop, spaces: [3] }] } },
    'POST /api/x/integrations/payments/request': {
      status: 404,
      body: { error: 'x', code: 'app_integration_slot_unknown', params: { slot: 'payments' } },
    },
  })
  const changed = panel(appOf(), team, true)
  const { confirm } = await openApproveAndPublish()
  const before = reads
  fireEvent.click(confirm)
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Approve and publish' })).toBeNull())
  expect(changed).toHaveBeenCalled()
  await waitFor(() => expect(reads).toBeGreaterThan(before))
  expect(screen.getByText(/The draft changed while you were reviewing it/)).toBeTruthy()
  expect(sent(calls, 'POST', '/api/x/publish')).toEqual([])
})

test('approvals made before a publish that waits for the schema changes are said when it is cancelled', async () => {
  let approved = false
  fakeFetch({
    'GET /api/x/integrations': () =>
      status(approved ? slot({ status: 'approved', integration: approvedShop }) : slot()),
    'GET /api/workspaces/1/integrations': { body: { integrations: [{ ...shop, spaces: [3] }] } },
    'POST /api/x/integrations/payments/request': { body: { id: 4 } },
    'POST /api/workspaces/1/integration-approvals/4/approve': () => {
      approved = true
      return { body: { id: 4 } }
    },
    'POST /api/x/publish': {
      status: 409,
      body: {
        error: 'x',
        code: 'app_migration_required',
        params: { changes: [{ change: 'field_removed', field: 'total' }] },
      },
    },
  })
  panel(appOf(), team, true)
  const { confirm } = await openApproveAndPublish()
  fireEvent.click(confirm)
  const migration = await screen.findByRole('dialog', { name: /./, description: undefined } as never).catch(() => null)
  expect(migration).not.toBeNull()
  fireEvent.click(await screen.findByRole('button', { name: 'Cancel' }))
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  expect(await screen.findByText(/These approvals went through and stay active: payments\./)).toBeTruthy()
})

test('a refused approval reads the integrations again, so one switched off is no longer offered', async () => {
  let reads = 0
  const calls = fakeFetch({
    'GET /api/x/integrations': status(slot()),
    'GET /api/workspaces/1/integrations': () => {
      reads++
      return { body: { integrations: [{ ...shop, disabled: reads > 1 }] } }
    },
    'POST /api/x/integrations/payments/request': { body: { id: 4 } },
    'POST /api/workspaces/1/integration-approvals/4/approve': {
      status: 409,
      body: { error: 'x', code: 'app_integration_disabled' },
    },
  })
  panel(appOf(), team, true)
  const { dialog, confirm } = await openApproveAndPublish()
  fireEvent.click(confirm)
  expect(await within(dialog).findByText('This integration is switched off.')).toBeTruthy()
  expect(await within(dialog).findByText('No integration for Payments is connected and switched on yet.')).toBeTruthy()
  expect(calls.filter((c) => c.path === '/api/workspaces/1/integrations')).toHaveLength(2)
  expect((within(dialog).getByRole('button', { name: 'Approve and publish' }) as HTMLButtonElement).disabled).toBe(true)
})

test('when a later slot is refused, the dialog says the earlier approvals went through and a retry leaves them alone', async () => {
  let firstDone = false
  let refusedOnce = false
  let secondOk = false
  const calls = fakeFetch({
    'GET /api/x/integrations': () =>
      status(
        firstDone ? slot({ status: 'approved', integration: approvedShop }) : slot(),
        secondOk ? { ...billing(), status: 'approved', integration: approvedShop } : billing(),
      ),
    'GET /api/workspaces/1/integrations': { body: { integrations: [{ ...shop, spaces: [3] }] } },
    'POST /api/x/integrations/payments/request': { body: { id: 4 } },
    'POST /api/x/integrations/billing/request': { body: { id: 5 } },
    'POST /api/workspaces/1/integration-approvals/4/approve': () => {
      firstDone = true
      return { body: { id: 4 } }
    },
    'POST /api/workspaces/1/integration-approvals/5/approve': () => {
      // Refused the first time, accepted the second.
      if (!refusedOnce) {
        refusedOnce = true
        return { status: 409, body: { error: 'x', code: 'app_integration_disabled' } }
      }
      secondOk = true
      return { body: { id: 5 } }
    },
    'POST /api/x/publish': { body: { id: 9, live: { number: 1 } } },
  })
  panel(appOf(), team, true)
  const { dialog, confirm } = await openApproveAndPublish()
  fireEvent.click(confirm)
  expect(await within(dialog).findByText('This integration is switched off.')).toBeTruthy()
  expect(within(dialog).getByText(/These approvals went through and stay active: payments\./)).toBeTruthy()
  expect(sent(calls, 'POST', '/api/x/publish')).toEqual([])
  // Only the slot that is left is offered again, and approving it does not touch the first.
  await waitFor(() => expect(within(dialog).queryByRole('region', { name: 'Service “payments”' })).toBeNull())
  expect(within(dialog).getByRole('region', { name: 'Service “billing”' })).toBeTruthy()
  const again = within(dialog).getByRole('button', { name: 'Approve and publish' }) as HTMLButtonElement
  await waitFor(() => expect(again.disabled).toBe(false))
  fireEvent.click(again)
  await waitFor(() => expect(sent(calls, 'POST', '/api/x/publish')).toHaveLength(1))
  expect(sent(calls, 'POST', '/api/workspaces/1/integration-approvals/4/approve')).toHaveLength(1)
})

test('when the publish is refused after every approval, the dialog says they went through and has nothing left to approve', async () => {
  let approved = false
  const calls = fakeFetch({
    'GET /api/x/integrations': () =>
      status(approved ? slot({ status: 'approved', integration: approvedShop }) : slot()),
    'GET /api/workspaces/1/integrations': { body: { integrations: [{ ...shop, spaces: [3] }] } },
    'POST /api/x/integrations/payments/request': { body: { id: 4 } },
    'POST /api/workspaces/1/integration-approvals/4/approve': () => {
      approved = true
      return { body: { id: 4 } }
    },
    'POST /api/x/publish': { status: 409, body: { error: 'x', code: 'app_changed' } },
  })
  panel(appOf(), team, true)
  const { dialog, confirm } = await openApproveAndPublish()
  fireEvent.click(confirm)
  expect(await within(dialog).findByText(/These approvals went through and stay active: payments\./)).toBeTruthy()
  // No empty dialog with a live button: it says why there is nothing to approve, and the button is off.
  expect(
    await within(dialog).findByText(/Everything this app asks of outside services is approved already/),
  ).toBeTruthy()
  expect((within(dialog).getByRole('button', { name: 'Approve and publish' }) as HTMLButtonElement).disabled).toBe(true)
  expect(within(dialog).queryByRole('region')).toBeNull()
  expect(sent(calls, 'POST', '/api/x/publish')).toHaveLength(1)
})

test('a draft that changed while the approvals were made is not published at the version they started on', async () => {
  let release: (() => void) | undefined
  let granted = false
  const calls = fakeFetch({
    'GET /api/x/integrations': () => status(granted ? slot({ status: 'approved', integration: approvedShop }) : slot()),
    'GET /api/workspaces/1/integrations': { body: { integrations: [{ ...shop, spaces: [3] }] } },
    'POST /api/x/integrations/payments/request': { body: { id: 4 } },
    'POST /api/workspaces/1/integration-approvals/4/approve': () =>
      new Promise((resolve) => {
        release = () => {
          granted = true
          resolve({ body: { id: 4 } })
        }
      }),
    'POST /api/x/publish': { body: { id: 9, live: { number: 1 } } },
  })
  const render1 = (app: App) => (
    <WorkspaceAdmin value={true}>
      <PublishPanel base="/api/x" spaceBase="/api/y" app={app} space={team} onChanged={() => {}} />
    </WorkspaceAdmin>
  )
  const view = render(render1(appOf()))
  const { confirm } = await openApproveAndPublish()
  fireEvent.click(confirm)
  await waitFor(() => expect(release).toBeDefined())
  // Someone changes the draft while the approval is on its way.
  view.rerender(render1(appOf({ version: 3 })))
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Approve and publish' })).toBeNull())
  release?.()
  await waitFor(() => expect(screen.getByText(/These approvals went through and stay active: payments\./)).toBeTruthy())
  expect(screen.getByText(/The draft changed while you were reviewing it/)).toBeTruthy()
  expect(sent(calls, 'POST', '/api/x/publish')).toEqual([])
})

test('the slots show the shape of a list while they are read', async () => {
  let release: (() => void) | undefined
  fakeFetch({
    'GET /api/x/integrations': () =>
      new Promise((resolve) => {
        release = () => resolve(status(slot()))
      }),
  })
  panel(appOf())
  const loading = await screen.findAllByRole('status')
  expect(loading.some((l) => l.textContent?.includes('Loading'))).toBe(true)
  release?.()
  await screen.findByText('Not asked for yet')
})

test('publishing stays closed when the status of the outside services can’t be read', async () => {
  fakeFetch({ 'GET /api/x/integrations': { status: 500, body: { error: 'x', code: 'internal' } } })
  panel(appOf(), { ...team, kind: 'private' })
  await waitFor(() => expect(screen.getByRole('alert')).toBeTruthy())
  expect(screen.queryByRole('button', { name: /publish/i })).toBeNull()
})

test('a request that waits is read again when the person comes back, and not otherwise', async () => {
  const calls = fakeFetch({ 'GET /api/x/integrations': status(slot({ status: 'requested', approval_id: 4 })) })
  panel(appOf())
  await screen.findByText(/Waiting for an admin/)
  const reads = () => calls.filter((c) => c.method === 'GET' && c.path === '/api/x/integrations').length
  expect(reads()).toBe(1)
  window.dispatchEvent(new Event('focus'))
  await waitFor(() => expect(reads()).toBe(2))
  cleanup()
  const other = fakeFetch({ 'GET /api/x/integrations': status(slot()) })
  panel(appOf())
  await screen.findByText('Not asked for yet')
  window.dispatchEvent(new Event('focus'))
  expect(other.filter((c) => c.path === '/api/x/integrations')).toHaveLength(1)
})
