// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type { Integration, IntegrationRegistry, Me, Space, Workspace } from '../api'
import { type Call, fakeFetch } from '../test/fakes'
import { SettingsPage } from './SettingsPage'

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [],
  can_create_workspace: false,
  is_operator: false,
}
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }
const member: Workspace = { id: 1, name: 'Home', role: 'member' }

const registry = (over: Partial<IntegrationRegistry> = {}): IntegrationRegistry => ({
  providers: [
    {
      id: 'stripe',
      kinds: ['payments'],
      secrets: [
        { name: 'api_key', required: true },
        { name: 'webhook_secret', required: false },
      ],
      settings: [],
    },
    {
      id: 'square',
      kinds: ['payments'],
      secrets: [{ name: 'api_key', required: true }],
      settings: [
        { name: 'environment', options: ['sandbox', 'production'], required: true },
        { name: 'location_id', options: [], required: true },
      ],
    },
  ],
  currencies: ['COP', 'EUR', 'JPY', 'USD'],
  // The server's own decimals, which the page uses: the browser's table says 0 for the peso.
  exponents: { COP: 2, EUR: 2, JPY: 0, USD: 2 },
  max_amount_minor: 1_000_000_000_000,
  webhooks_available: false,
  ...over,
})

const shop: Integration = {
  id: 7,
  provider: 'stripe',
  kind: 'payments',
  name: 'shop',
  display_name: 'Shop',
  mode: 'test',
  settings: {},
  max_amount_minor: 500000,
  currencies: ['EUR', 'JPY'],
  secrets: { api_key: 'set', webhook_secret: 'not set' },
  webhook_url: null,
  disabled: false,
  disabled_at: null,
  spaces: [12],
  other_spaces: 0,
  created_at: '2026-11-24T10:00:00.000Z',
  updated_at: '2026-11-24T10:00:00.000Z',
  last_event_at: null,
}

const spaces: Space[] = [
  { id: 11, workspace_id: 1, kind: 'private', name: 'Private', archived_at: null, role: 'owner', access: 'write' },
  { id: 12, workspace_id: 1, kind: 'shared', name: 'Shop floor', archived_at: null, role: 'editor', access: 'write' },
  { id: 13, workspace_id: 1, kind: 'shared', name: 'Old', archived_at: '2026-01-01T00:00:00.000Z', access: 'none' },
]

const routes = (list: Integration[] = [shop], reg = registry()) => ({
  'GET /api/workspaces/1/integration-providers': { body: reg },
  'GET /api/workspaces/1/integrations': { body: { integrations: list } },
  'GET /api/workspaces/1/spaces?all=true': { body: { spaces } },
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const sent = (calls: Call[], method: string, path: string) =>
  calls.filter((c) => c.method === method && c.path === path).map((c) => c.body)

test('the integrations tab is the admin’s', async () => {
  fakeFetch(routes())
  render(<SettingsPage me={me} workspace={member} tab="integrations" />)
  expect(screen.queryByRole('link', { name: 'Integrations' })).toBeNull()
  expect(screen.queryByRole('heading', { name: 'Integrations' })).toBeNull()
})

test('an integration is listed by what it is, with its secrets only said to be set', async () => {
  fakeFetch(routes())
  render(<SettingsPage me={me} workspace={admin} tab="integrations" />)
  const row = await screen.findByRole('listitem', { name: 'Shop' })
  expect(within(row).getByText(/Stripe · Payments · Test mode/)).toBeTruthy()
  expect(within(row).getByText(/Secret key set · Webhook signing secret not set/)).toBeTruthy()
  // The same number of smallest units is not the same sum in euros and in yen.
  expect(within(row).getByText(/€5,000\.00 and ¥500,000 per payment/)).toBeTruthy()
  expect(within(row).getByText('Allowed in: Shop floor')).toBeTruthy()
  expect(within(row).getByText(/No public URL: payments are reconciled by polling/)).toBeTruthy()
  expect(screen.getByRole('status').textContent).toContain('Webhooks unavailable')
})

test('an amount is written with the server’s decimals, not the browser’s', async () => {
  fakeFetch(routes([{ ...shop, currencies: ['COP'], max_amount_minor: 500000 }]))
  render(<SettingsPage me={me} workspace={admin} tab="integrations" />)
  const row = await screen.findByRole('listitem', { name: 'Shop' })
  // The server counts pesos in hundredths; Intl alone would say 500,000 whole pesos.
  expect(within(row).getByText(/COP\s?5,000\.00 per payment/)).toBeTruthy()
})

test('a private space of another admin is only counted, never named', async () => {
  fakeFetch(routes([{ ...shop, other_spaces: 2 }]))
  render(<SettingsPage me={me} workspace={admin} tab="integrations" />)
  const row = await screen.findByRole('listitem', { name: 'Shop' })
  expect(within(row).getByText(/Allowed in: Shop floor/).textContent).toContain('And 2 private spaces of other members')
  cleanup()
  fakeFetch(routes([{ ...shop, spaces: [], other_spaces: 1 }]))
  render(<SettingsPage me={me} workspace={admin} tab="integrations" />)
  const only = await screen.findByRole('listitem', { name: 'Shop' })
  expect(within(only).getByText(/And 1 private space of another member/)).toBeTruthy()
  expect(within(only).queryByText(/Not allowed in any space yet/)).toBeNull()
})

test('with a public URL the row shows the address to register', async () => {
  fakeFetch(
    routes(
      [{ ...shop, webhook_url: 'https://tasks.example.com/hooks/0123456789abcdef0123456789abcdef' }],
      registry({ webhooks_available: true }),
    ),
  )
  render(<SettingsPage me={me} workspace={admin} tab="integrations" />)
  const row = await screen.findByRole('listitem', { name: 'Shop' })
  expect(within(row).getByText('https://tasks.example.com/hooks/0123456789abcdef0123456789abcdef')).toBeTruthy()
  expect(screen.queryByRole('status')).toBeNull()
})

test('an admin connects a Stripe account: the key is sent once and the form is cleared', async () => {
  const calls = fakeFetch({ ...routes([]), 'POST /api/workspaces/1/integrations': { status: 201, body: shop } })
  render(<SettingsPage me={me} workspace={admin} tab="integrations" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Connect an account' }))
  const form = screen.getByRole('form', { name: 'New integration' })
  fireEvent.change(within(form).getByLabelText('Name'), { target: { value: 'shop' } })
  fireEvent.change(within(form).getByLabelText('Secret key'), { target: { value: 'fake-key-for-the-form' } })
  fireEvent.change(within(form).getByLabelText('Largest amount of one payment'), { target: { value: '500000' } })
  // A currency is needed before the form can be sent.
  const add = within(form).getByRole('button', { name: 'Add' }) as HTMLButtonElement
  expect(add.disabled).toBe(true)
  fireEvent.click(within(form).getByRole('checkbox', { name: 'EUR' }))
  fireEvent.click(within(form).getByRole('checkbox', { name: 'JPY' }))
  expect(within(form).getByText(/That is €5,000\.00 and ¥500,000/)).toBeTruthy()
  fireEvent.click(add)
  await waitFor(() => expect(sent(calls, 'POST', '/api/workspaces/1/integrations')).toHaveLength(1))
  expect(sent(calls, 'POST', '/api/workspaces/1/integrations')[0]).toEqual({
    provider: 'stripe',
    name: 'shop',
    display_name: '',
    settings: {},
    secrets: { api_key: 'fake-key-for-the-form' },
    max_amount_minor: 500000,
    currencies: ['EUR', 'JPY'],
  })
  await waitFor(() => expect(screen.queryByRole('form', { name: 'New integration' })).toBeNull())
})

test("Square's form asks for its environment and location, as the registry says", async () => {
  const calls = fakeFetch({ ...routes([]), 'POST /api/workspaces/1/integrations': { status: 201, body: shop } })
  render(<SettingsPage me={me} workspace={admin} tab="integrations" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Connect an account' }))
  const form = screen.getByRole('form', { name: 'New integration' })
  fireEvent.change(within(form).getByLabelText('Provider'), { target: { value: 'square' } })
  expect(within(form).queryByLabelText('Webhook signing secret')).toBeNull()
  fireEvent.change(within(form).getByLabelText('Name'), { target: { value: 'square-shop' } })
  fireEvent.change(within(form).getByLabelText('Environment'), { target: { value: 'sandbox' } })
  fireEvent.change(within(form).getByLabelText('Location ID'), { target: { value: 'L123' } })
  fireEvent.change(within(form).getByLabelText('Secret key'), { target: { value: 'fake-square-token-for-the-form' } })
  fireEvent.change(within(form).getByLabelText('Largest amount of one payment'), { target: { value: '1000' } })
  fireEvent.click(within(form).getByRole('checkbox', { name: 'USD' }))
  fireEvent.click(within(form).getByRole('button', { name: 'Add' }))
  await waitFor(() => expect(sent(calls, 'POST', '/api/workspaces/1/integrations')).toHaveLength(1))
  expect(sent(calls, 'POST', '/api/workspaces/1/integrations')[0]).toMatchObject({
    provider: 'square',
    settings: { environment: 'sandbox', location_id: 'L123' },
    secrets: { api_key: 'fake-square-token-for-the-form' },
  })
})

test('editing keeps a stored secret unless a new one is typed, and never shows it', async () => {
  const calls = fakeFetch({ ...routes(), 'PATCH /api/workspaces/1/integrations/7': { body: shop } })
  render(<SettingsPage me={me} workspace={admin} tab="integrations" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Edit Shop' }))
  const form = screen.getByRole('form', { name: 'Edit Shop' })
  const key = within(form).getByLabelText('New Secret key') as HTMLInputElement
  expect(key.value).toBe('')
  expect(key.required).toBe(false)
  expect(within(form).queryByLabelText('Name')).toBeNull()
  fireEvent.change(within(form).getByLabelText('Largest amount of one payment'), { target: { value: '90000' } })
  fireEvent.click(within(form).getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(sent(calls, 'PATCH', '/api/workspaces/1/integrations/7')).toHaveLength(1))
  const body = sent(calls, 'PATCH', '/api/workspaces/1/integrations/7')[0] as Record<string, unknown>
  expect(body.secrets).toBeUndefined()
  expect(body.max_amount_minor).toBe(90000)
  expect(body.currencies).toEqual(['EUR', 'JPY'])
})

test('a check shows ok, or the code of what is wrong, never anything of the provider', async () => {
  let answer: unknown = { ok: true, remote: false }
  fakeFetch({ ...routes(), 'POST /api/workspaces/1/integrations/7/check': () => ({ body: answer }) })
  render(<SettingsPage me={me} workspace={admin} tab="integrations" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Test Shop' }))
  expect((await screen.findByText(/The stored credentials open/)).textContent).toContain("The provider wasn't called")
  answer = { ok: false, remote: false, code: 'integration_secret_unreadable', params: { secret: 'api_key' } }
  fireEvent.click(screen.getByRole('button', { name: 'Test Shop' }))
  expect(await screen.findByText(/can't be read, because the server's key has changed/)).toBeTruthy()
})

test('an admin chooses the spaces, and only the ones that can be allowed are offered', async () => {
  const calls = fakeFetch({ ...routes(), 'PUT /api/workspaces/1/integrations/7/spaces': { body: shop } })
  render(<SettingsPage me={me} workspace={admin} tab="integrations" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Spaces of Shop' }))
  const form = screen.getByRole('form', { name: 'Spaces of Shop' })
  // An archived space the integration isn't allowed in can't be chosen.
  expect(within(form).queryByRole('checkbox', { name: /Old/ })).toBeNull()
  expect((within(form).getByRole('checkbox', { name: 'Shop floor' }) as HTMLInputElement).checked).toBe(true)
  fireEvent.click(within(form).getByRole('checkbox', { name: 'Private (private)' }))
  fireEvent.click(within(form).getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(sent(calls, 'PUT', '/api/workspaces/1/integrations/7/spaces')).toHaveLength(1))
  expect(sent(calls, 'PUT', '/api/workspaces/1/integrations/7/spaces')[0]).toEqual({ spaces: [12, 11] })
})

test('an archived space the integration is still allowed in is listed, marked, and can be taken out', async () => {
  const calls = fakeFetch({
    ...routes([{ ...shop, spaces: [12, 13] }]),
    'PUT /api/workspaces/1/integrations/7/spaces': { body: shop },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integrations" />)
  const row = await screen.findByRole('listitem', { name: 'Shop' })
  expect(within(row).getByText('Allowed in: Shop floor and Old (archived)')).toBeTruthy()
  fireEvent.click(within(row).getByRole('button', { name: 'Spaces of Shop' }))
  const form = screen.getByRole('form', { name: 'Spaces of Shop' })
  const old = within(form).getByRole('checkbox', { name: 'Old (archived)' }) as HTMLInputElement
  expect(old.checked).toBe(true)
  fireEvent.click(old)
  fireEvent.click(within(form).getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(sent(calls, 'PUT', '/api/workspaces/1/integrations/7/spaces')).toHaveLength(1))
  expect(sent(calls, 'PUT', '/api/workspaces/1/integrations/7/spaces')[0]).toEqual({ spaces: [12] })
})

test('a provider change drops what was typed for the last one, and a check says so only while it runs', async () => {
  const calls = fakeFetch({ ...routes([]), 'POST /api/workspaces/1/integrations': { status: 201, body: shop } })
  render(<SettingsPage me={me} workspace={admin} tab="integrations" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Connect an account' }))
  const form = screen.getByRole('form', { name: 'New integration' })
  // A webhook secret typed for Stripe is not sent to Square, which has no such field.
  fireEvent.change(within(form).getByLabelText('Webhook signing secret'), {
    target: { value: 'fake-hook-for-the-form' },
  })
  fireEvent.change(within(form).getByLabelText('Provider'), { target: { value: 'square' } })
  fireEvent.change(within(form).getByLabelText('Name'), { target: { value: 'square-shop' } })
  fireEvent.change(within(form).getByLabelText('Environment'), { target: { value: 'sandbox' } })
  fireEvent.change(within(form).getByLabelText('Location ID'), { target: { value: 'L123' } })
  fireEvent.change(within(form).getByLabelText('Secret key'), { target: { value: 'fake-square-token-for-the-form' } })
  fireEvent.change(within(form).getByLabelText('Largest amount of one payment'), { target: { value: '1000' } })
  fireEvent.click(within(form).getByRole('checkbox', { name: 'USD' }))
  fireEvent.click(within(form).getByRole('button', { name: 'Add' }))
  await waitFor(() => expect(sent(calls, 'POST', '/api/workspaces/1/integrations')).toHaveLength(1))
  expect((sent(calls, 'POST', '/api/workspaces/1/integrations')[0] as { secrets: unknown }).secrets).toEqual({
    api_key: 'fake-square-token-for-the-form',
  })
})

test('"Checking…" is shown only for a check, not while switching off or removing', async () => {
  let release: () => void = () => {}
  const gate = new Promise<void>((resolve) => {
    release = resolve
  })
  fakeFetch({
    ...routes(),
    'PATCH /api/workspaces/1/integrations/7': async () => {
      await gate
      return { body: { ...shop, disabled: true } }
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integrations" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Switch off Shop' }))
  await waitFor(() =>
    expect((screen.getByRole('button', { name: 'Test Shop' }) as HTMLButtonElement).disabled).toBe(true),
  )
  expect(screen.queryByText('Checking…')).toBeNull()
  release()
})

test('switching off and removing ask the server, and removing asks first', async () => {
  const calls = fakeFetch({
    ...routes(),
    'PATCH /api/workspaces/1/integrations/7': { body: { ...shop, disabled: true } },
    'DELETE /api/workspaces/1/integrations/7': { status: 204 },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integrations" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Switch off Shop' }))
  await waitFor(() => expect(sent(calls, 'PATCH', '/api/workspaces/1/integrations/7')).toEqual([{ disabled: true }]))

  fireEvent.click(screen.getByRole('button', { name: 'Remove Shop' }))
  expect(sent(calls, 'DELETE', '/api/workspaces/1/integrations/7')).toHaveLength(0)
  expect(screen.getByText(/Its credentials are deleted with it/)).toBeTruthy()
  fireEvent.click(screen.getByRole('button', { name: 'Yes, remove' }))
  await waitFor(() => expect(sent(calls, 'DELETE', '/api/workspaces/1/integrations/7')).toHaveLength(1))
})

test('an error of the server is shown in words, from its code', async () => {
  fakeFetch({
    ...routes(),
    'DELETE /api/workspaces/1/integrations/7': {
      status: 409,
      body: { error: 'the integration is in use', code: 'integration_in_use' },
    },
  })
  render(<SettingsPage me={me} workspace={admin} tab="integrations" />)
  fireEvent.click(await screen.findByRole('button', { name: 'Remove Shop' }))
  fireEvent.click(screen.getByRole('button', { name: 'Yes, remove' }))
  expect(await screen.findByText(/An app still uses this integration/)).toBeTruthy()
})
