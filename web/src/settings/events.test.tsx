// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import type {
  IntegrationEvent,
  IntegrationRegistry,
  Me,
  PaymentLinkAttempt,
  PaymentLinkOutcome,
  Space,
  Workspace,
} from '../api'
import { type Call, fakeFetch } from '../test/fakes'
import { shop } from '../test/integrations'
import { SettingsPage } from './SettingsPage'

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [],
  can_create_workspace: false,
  is_operator: false,
}
const admin: Workspace = { id: 1, name: 'Home', role: 'admin' }
const member: Workspace = { id: 1, name: 'Home', role: 'member' }

const base = '/api/workspaces/1/integrations/7'
const registry: IntegrationRegistry = {
  providers: [],
  currencies: ['EUR', 'JPY'],
  exponents: { EUR: 2, JPY: 0 },
  max_amount_minor: 100000000,
  webhooks_available: true,
}
const space = (id: number, name: string): Space => ({
  id,
  workspace_id: 1,
  kind: 'shared',
  name,
  archived_at: null,
  role: 'owner',
  access: 'write',
})

const event = (over: Partial<IntegrationEvent> = {}): IntegrationEvent => ({
  id: 1,
  kind: 'payment.succeeded',
  source: 'webhook',
  state: 'applied',
  amount_minor: 12000,
  currency: 'EUR',
  livemode: false,
  attempts: 1,
  occurred_at: '2026-11-26T12:00:00Z',
  received_at: '2026-11-26T12:00:01Z',
  processed_at: '2026-11-26T12:00:02Z',
  retriable: false,
  ...over,
})
const link = (over: Partial<PaymentLinkOutcome> = {}): PaymentLinkOutcome => ({
  effect_id: 31,
  app_id: 4,
  space_id: 12,
  record_id: 88,
  amount: '120.00',
  currency: 'EUR',
  since: '2026-11-26T12:00:00Z',
  waiting: false,
  can_confirm: true,
  object_id: '',
  attempts: [],
  ...over,
})
// A reference is built at run time, as the server's are random: no literal that looks like a token.
const reference = (digit: string) => digit.repeat(32)
const attempt = (over: Partial<PaymentLinkAttempt> = {}): PaymentLinkAttempt => ({
  reference: reference('a'),
  kind: 'checkout_session',
  object_id: '',
  object_verified: false,
  account_id: 'acct_shop',
  mode: 'test',
  first_sent_at: '2026-11-26T12:00:00Z',
  ...over,
})

// What the server answers around the page: the workspace's integrations, its spaces and the registry.
const around = {
  'GET /api/workspaces/1/integrations': { body: { integrations: [shop] } },
  'GET /api/workspaces/1/integration-providers': { body: registry },
  'GET /api/workspaces/1/spaces?all=true': { body: { spaces: [space(12, 'Shop floor')] } },
}
const events = (...items: IntegrationEvent[]) => ({
  [`GET ${base}/events`]: { body: { events: items, next_before: 0 } },
})
const links = (...items: PaymentLinkOutcome[]) => ({
  [`GET ${base}/payment-links/unresolved`]: { body: { payment_links: items } },
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const open = (workspace: Workspace = admin) =>
  render(<SettingsPage me={me} workspace={workspace} tab="integration-events" />)
const sent = (calls: Call[], method: string, path: string) =>
  calls.filter((c) => c.method === method && c.path === path).map((c) => c.body)

test('the events tab is the admin’s: a member sees no tab and nothing is asked for', async () => {
  const calls = fakeFetch({ ...around, ...events(event()), ...links() })
  open(member)
  expect(screen.queryByRole('link', { name: 'Integration events' })).toBeNull()
  expect(screen.queryByRole('heading', { name: 'Integration events' })).toBeNull()
  expect(calls.filter((c) => c.path.includes('/events') || c.path.includes('payment-links'))).toEqual([])
  cleanup()
  fakeFetch({ ...around, ...events(event()), ...links() })
  open()
  expect(screen.getByRole('link', { name: 'Integration events' })).toBeTruthy()
  expect(await screen.findByRole('heading', { name: 'Integration events' })).toBeTruthy()
})

test('each state is shown with its reason, the amount as money and the time of the event', async () => {
  fakeFetch({
    ...around,
    ...events(
      event({ id: 6, state: 'pending', kind: 'payment.expired' }),
      event({ id: 5, state: 'failed', code: 'integration_event_record_gone', retriable: true, attempts: 3 }),
      event({ id: 4, state: 'failed', code: 'integration_amount_mismatch', retriable: true, amount_minor: 5000 }),
      event({ id: 3, state: 'ignored', code: 'integration_event_second_payment' }),
      event({ id: 2, state: 'ignored', code: 'integration_event_no_handler', kind: 'payment.refunded' }),
      event({ id: 1, state: 'applied', livemode: true }),
    ),
    ...links(),
  })
  open()
  const list = await screen.findByRole('list', { name: 'Events' })
  const rows = within(list).getAllByRole('listitem')
  expect(rows).toHaveLength(6)
  const [waiting, gone, mismatch, duplicate, ignored, applied] = rows.map((r) => r.textContent ?? '')

  expect(waiting).toContain('Payment link expired')
  expect(waiting).toContain('Waiting')
  expect(gone).toContain('Failed')
  expect(gone).toContain('A payment arrived for a record that no longer exists. Settle the payment by hand.')
  expect(gone).toContain('Tried 3 times.')
  expect(mismatch).toContain('Amount mismatch')
  expect(mismatch).toContain('€50.00')
  expect(mismatch).toContain('A payment arrived for another amount or currency than the payment link asked.')
  expect(duplicate).toContain('Duplicate payment')
  expect(duplicate).toContain('The payment link was paid a second time.')
  expect(ignored).toContain('Ignored')
  expect(ignored).toContain('Payment refunded')
  expect(ignored).toContain('The app declares nothing for this event, so its record was left as it is.')
  expect(applied).toContain('Applied')
  expect(applied).toContain('Payment received')
  expect(applied).toContain('€120.00')
  expect(applied).toContain('Nov 26, 2026')
  expect(applied).toContain('Live money')
  expect(gone).toContain('Test mode')
})

test('a refund that waits for its payment is shown waiting, not failed, with no retry', async () => {
  fakeFetch({
    ...around,
    ...events(event({ id: 2, state: 'pending', kind: 'payment.refunded', code: 'integration_event_payment_unknown' })),
    ...links(),
  })
  open()
  const list = await screen.findByRole('list', { name: 'Events' })
  const [row] = within(list).getAllByRole('listitem')
  const text = row.textContent ?? ''
  expect(text).toContain('Payment refunded')
  expect(text).toContain('Waiting')
  expect(text).not.toContain('Failed')
  expect(text).toContain('A refund arrived for a payment that no payment link has been paid by yet.')
  expect(within(row).queryByRole('button', { name: /Retry/ })).toBeNull()
})

test('an event says nothing of the record it touched: the server holds none, so none is asked for or shown', async () => {
  const calls = fakeFetch({
    ...around,
    ...events(
      // Even a server that sent more than it should would not have it shown.
      { ...event({ state: 'applied' }), record_id: 9917, app_id: 4417, space_id: 8817 } as IntegrationEvent,
    ),
    ...links(),
  })
  open()
  await screen.findByRole('list', { name: 'Events' })
  const text = document.body.textContent ?? ''
  for (const leaked of ['9917', '4417', '8817']) expect(text).not.toContain(leaked)
  // Nothing outside this workspace's integration, its spaces' names and the registry was read.
  expect(calls.map((c) => `${c.method} ${c.path}`).sort()).toEqual(
    [
      'GET /api/workspaces/1/integration-providers',
      'GET /api/workspaces/1/integrations',
      `GET ${base}/events`,
      `GET ${base}/payment-links/unresolved`,
      'GET /api/workspaces/1/spaces?all=true',
    ].sort(),
  )
})

test('a filter asks the server for that state, and older events continue after the last one shown', async () => {
  const calls = fakeFetch({
    ...around,
    [`GET ${base}/events`]: { body: { events: [event({ id: 9 }), event({ id: 8 })], next_before: 8 } },
    [`GET ${base}/events?before=8`]: { body: { events: [event({ id: 7, kind: 'payment.failed' })], next_before: 0 } },
    [`GET ${base}/events?state=failed`]: {
      body: {
        events: [event({ id: 5, state: 'failed', code: 'integration_event_app_offline', retriable: true })],
        next_before: 0,
      },
    },
    ...links(),
  })
  open()
  const list = await screen.findByRole('list', { name: 'Events' })
  expect(within(list).getAllByRole('listitem')).toHaveLength(2)
  fireEvent.click(screen.getByRole('button', { name: 'Show older events' }))
  await waitFor(() =>
    expect(within(screen.getByRole('list', { name: 'Events' })).getAllByRole('listitem')).toHaveLength(3),
  )
  expect(screen.getByText('Payment failed')).toBeTruthy()
  expect(screen.queryByRole('button', { name: 'Show older events' })).toBeNull()

  fireEvent.click(screen.getByRole('radio', { name: 'Failed' }))
  const failed = await screen.findByText(/the app is offline, not published or in an archived space/)
  expect(failed).toBeTruthy()
  expect(calls.some((c) => c.path === `${base}/events?state=failed`)).toBe(true)
})

test('an empty list says so', async () => {
  fakeFetch({ ...around, ...events(), ...links() })
  open()
  expect(await screen.findByText('No events have arrived yet.')).toBeTruthy()
  expect(await screen.findByText('No payment link is waiting for a decision.')).toBeTruthy()
})

test('retrying a failed event calls its route, says so, and reads the list again; only a failed event has the button', async () => {
  let retried = false
  const calls = fakeFetch({
    ...around,
    [`GET ${base}/events`]: () => ({
      body: {
        events: [
          event({ id: 2, state: 'failed', code: 'integration_event_app_offline', retriable: !retried }),
          event({ id: 1 }),
        ].map((e) => (retried && e.id === 2 ? { ...e, state: 'pending', code: undefined, retriable: false } : e)),
        next_before: 0,
      },
    }),
    [`POST ${base}/events/2/retry`]: () => {
      retried = true
      return { body: event({ id: 2, state: 'pending' }) }
    },
    ...links(),
  })
  open()
  const list = await screen.findByRole('list', { name: 'Events' })
  expect(within(list).getAllByRole('button', { name: /^Retry the event/ })).toHaveLength(1)
  fireEvent.click(within(list).getByRole('button', { name: /^Retry the event “Payment received” of Nov 26, 2026/ }))
  expect(await screen.findByText('“Payment received” will be applied again in a moment.')).toBeTruthy()
  expect(sent(calls, 'POST', `${base}/events/2/retry`)).toEqual([{}])
  await waitFor(() => expect(screen.queryByRole('button', { name: /^Retry the event/ })).toBeNull())
  expect(within(screen.getByRole('list', { name: 'Events' })).getByText('Waiting')).toBeTruthy()
})

test('a refused retry that came too late is said in the section, and the event that is no longer failed is read again', async () => {
  let reads = 0
  fakeFetch({
    ...around,
    [`GET ${base}/events`]: () => {
      reads++
      return {
        body: {
          events: [
            reads === 1
              ? event({ id: 2, state: 'failed', code: 'integration_event_app_offline', retriable: true })
              : event({ id: 2, state: 'applied' }),
          ],
          next_before: 0,
        },
      }
    },
    [`POST ${base}/events/2/retry`]: {
      status: 409,
      body: { error: 'only a failed event is retried', code: 'integration_event_not_retriable' },
    },
    ...links(),
  })
  open()
  fireEvent.click(await screen.findByRole('button', { name: /^Retry the event/ }))
  const refusal = await screen.findByText(/Only an event that failed can be retried\./)
  expect(refusal.closest('[role="alert"]')).toBeTruthy()
  // What the server holds now replaces the row that was out of date.
  await waitFor(() => expect(reads).toBe(2))
  await waitFor(() => expect(screen.queryByRole('button', { name: /^Retry the event/ })).toBeNull())
  expect(screen.getByText(/Only an event that failed can be retried\./)).toBeTruthy()
})

test('with the Failed filter the refusal stays after the row leaves the list, and goes with the next filter', async () => {
  let retried = false
  fakeFetch({
    ...around,
    ...events(),
    [`GET ${base}/events?state=failed`]: () => ({
      body: {
        events: retried
          ? []
          : [event({ id: 2, state: 'failed', code: 'integration_event_app_offline', retriable: true })],
        next_before: 0,
      },
    }),
    [`POST ${base}/events/2/retry`]: () => {
      retried = true
      return { status: 409, body: { error: 'only a failed event is retried', code: 'integration_event_not_retriable' } }
    },
    ...links(),
  })
  open()
  fireEvent.click(await screen.findByRole('radio', { name: 'Failed' }))
  fireEvent.click(await screen.findByRole('button', { name: /^Retry the event/ }))
  // The list is read again and no longer holds the event, which had a row for the refusal.
  expect(await screen.findByText('No event has failed.')).toBeTruthy()
  expect(screen.queryByRole('button', { name: /^Retry the event/ })).toBeNull()
  const refusal = screen.getByText(/Only an event that failed can be retried\./)
  expect(refusal.closest('[role="alert"]')).toBeTruthy()
  expect(refusal.textContent).toContain('“Payment received” of Nov 26, 2026')
  // Another filter is another question: the refusal is not carried to it.
  fireEvent.click(screen.getByRole('radio', { name: 'All' }))
  await waitFor(() => expect(screen.queryByText(/Only an event that failed can be retried\./)).toBeNull())
})

test('a refusal that leaves the event failed stays under it and is said in words, not as a status number', async () => {
  fakeFetch({
    ...around,
    ...events(event({ id: 2, state: 'failed', code: 'integration_event_app_offline', retriable: true })),
    [`POST ${base}/events/2/retry`]: { status: 500, body: { error: 'boom' } },
    ...links(),
  })
  open()
  const button = await screen.findByRole('button', { name: /^Retry the event/ })
  fireEvent.click(button)
  const alert = await screen.findByRole('alert')
  expect(alert.textContent).toBeTruthy()
  // Still failed, still retriable.
  expect(screen.getByRole('button', { name: /^Retry the event/ })).toBeTruthy()
})

test('the payment links to check say what to look for at Stripe; one still being looked for asks nothing of the admin', async () => {
  fakeFetch({
    ...around,
    ...events(),
    ...links(
      link(),
      link({ effect_id: 32, record_id: 90, amount: '75.50', waiting: true, can_confirm: false }),
      link({
        effect_id: 33,
        record_id: 91,
        amount: '12.00',
        waiting: false,
        can_confirm: false,
        code: 'integration_account_changed',
      }),
    ),
  })
  open()
  const list = await screen.findByRole('list', { name: 'Payment links to check' })
  const [needs, looking, stopped] = within(list).getAllByRole('listitem')
  expect(needs.textContent).toContain('Payment link for €120.00')
  expect(needs.textContent).toContain('Record 88 of app 4 · Shop floor')
  expect(needs.textContent).toContain('Outcome unknown since Nov 26, 2026')
  expect(within(needs).getByText('Needs you')).toBeTruthy()
  expect(within(needs).getByRole('button', { name: 'Confirm that no payment link of €120.00 exists' })).toBeTruthy()
  expect(looking.textContent).toContain('€75.50')
  expect(within(looking).getByText('Still looking')).toBeTruthy()
  expect(within(looking).queryByRole('button')).toBeNull()
  // A lookup that stopped is not "still looking": its own status, its reason in words and what to do.
  expect(within(stopped).getByText('Lookup stopped')).toBeTruthy()
  expect(within(stopped).queryByText('Still looking')).toBeNull()
  expect(within(stopped).queryByText('Needs you')).toBeNull()
  expect(stopped.textContent).toContain('started under another account of the payment provider')
  expect(stopped.textContent).toContain('Nothing can be confirmed here.')
  expect(within(stopped).queryByRole('button')).toBeNull()
  const check = screen.getByText('What to check at Stripe').closest('.ui-notice') as HTMLElement
  // Each kind of object carries TaskTo's reference in its own field.
  expect(check.textContent).toContain('For a Checkout Session, open its details and look for “client_reference_id”.')
  expect(check.textContent).toContain('For a Payment Link, look for “metadata[taskto_ref]”.')
  expect(check.textContent).not.toContain('whose metadata holds')
  expect(check.textContent).toContain('same mode, test or live')
  expect(check.textContent).toContain('every attempt, not only the last')
  expect(check.textContent).toContain('The person who ran the original action in the app, not the payer, retries it')
  expect(check.textContent).toContain('confirm below that none exists')
})

test('a payment link of a space the admin can’t name is shown by its number, never by a guess', async () => {
  fakeFetch({
    ...around,
    'GET /api/workspaces/1/spaces?all=true': { body: { spaces: [] } },
    ...events(),
    ...links(link({ space_id: 99 })),
  })
  open()
  const list = await screen.findByRole('list', { name: 'Payment links to check' })
  expect(list.textContent).toContain('Space 99')
})

test('confirming asks first, says it can make a second link, and cancelling sends nothing', async () => {
  const calls = fakeFetch({ ...around, ...events(), ...links(link()) })
  open()
  fireEvent.click(await screen.findByRole('button', { name: 'Confirm that no payment link of €120.00 exists' }))
  const dialog = await screen.findByRole('alertdialog', {
    name: 'Confirm that Stripe has no payment link for €120.00?',
  })
  const text = dialog.textContent ?? ''
  expect(text).toContain(
    'You are saying that you checked the Stripe account and found no payment link or checkout session',
  )
  expect(text).toContain('One more attempt to make a payment link will be queued, once.')
  expect(text).not.toContain('One new payment link will be made')
  expect(text).toContain('If one does exist and you missed it, the payer ends up with two links and could pay twice.')
  // The way out has the focus, so Enter doesn't confirm by habit.
  expect(document.activeElement).toBe(within(dialog).getByRole('button', { name: 'Cancel' }))
  fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
  await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
  expect(calls.filter((c) => c.method !== 'GET')).toEqual([])
})

test('confirming sends the decision once, says it, and reads the list again', async () => {
  let done = false
  const calls = fakeFetch({
    ...around,
    ...events(),
    [`GET ${base}/payment-links/unresolved`]: () => ({ body: { payment_links: done ? [] : [link()] } }),
    [`POST ${base}/payment-links/31/confirm-no-link`]: () => {
      done = true
      return { status: 204 }
    },
  })
  open()
  fireEvent.click(await screen.findByRole('button', { name: 'Confirm that no payment link of €120.00 exists' }))
  const dialog = await screen.findByRole('alertdialog')
  fireEvent.click(within(dialog).getByRole('button', { name: 'Confirm no link exists' }))
  const queued = await screen.findByText(/^Another attempt to make a payment link for €120\.00 is queued, once\./)
  expect(queued.textContent).toContain('the payer could end up with two links and pay twice')
  expect(sent(calls, 'POST', `${base}/payment-links/31/confirm-no-link`)).toEqual([{}])
  expect(await screen.findByText('No payment link is waiting for a decision.')).toBeTruthy()
})

test('a confirmation the server refuses says why and leaves the list as the server holds it', async () => {
  let reads = 0
  fakeFetch({
    ...around,
    ...events(),
    [`GET ${base}/payment-links/unresolved`]: () => {
      reads++
      return { body: { payment_links: reads === 1 ? [link()] : [] } }
    },
    [`POST ${base}/payment-links/31/confirm-no-link`]: {
      status: 409,
      body: { error: 'nothing to confirm', code: 'integration_nothing_to_confirm' },
    },
  })
  open()
  fireEvent.click(await screen.findByRole('button', { name: 'Confirm that no payment link of €120.00 exists' }))
  fireEvent.click(
    within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Confirm no link exists' }),
  )
  const refusal = await screen.findByText('This payment isn’t waiting for a decision about an unknown outcome.')
  expect(refusal.closest('[role="alert"]')).toBeTruthy()
  expect(screen.queryByText(/Another attempt to make a payment link/)).toBeNull()
  await waitFor(() => expect(reads).toBe(2))
  expect(await screen.findByText('No payment link is waiting for a decision.')).toBeTruthy()
})

test('with several integrations the admin picks one, and its events are the ones asked for; none shows its way to the settings', async () => {
  const other = { ...shop, id: 8, name: 'second', display_name: 'Second shop' }
  const calls = fakeFetch({
    ...around,
    'GET /api/workspaces/1/integrations': { body: { integrations: [shop, other] } },
    ...events(event()),
    ...links(),
    ['GET /api/workspaces/1/integrations/8/events']: {
      body: { events: [event({ id: 3, kind: 'payment.failed' })], next_before: 0 },
    },
    ['GET /api/workspaces/1/integrations/8/payment-links/unresolved']: { body: { payment_links: [] } },
  })
  open()
  await screen.findByRole('list', { name: 'Events' })
  fireEvent.change(screen.getByRole('combobox', { name: 'Integration' }), { target: { value: '8' } })
  expect(await screen.findByText('Payment failed')).toBeTruthy()
  expect(calls.some((c) => c.path === '/api/workspaces/1/integrations/8/events')).toBe(true)
  cleanup()
  fakeFetch({ ...around, 'GET /api/workspaces/1/integrations': { body: { integrations: [] } } })
  open()
  expect(await screen.findByText('There is no integration to show events for.')).toBeTruthy()
  expect(screen.getByRole('link', { name: 'Open integrations' })).toBeTruthy()
})

test('each payment lists its attempts with the reference, the field to search, any object seen, the account, the mode and the time', async () => {
  fakeFetch({
    ...around,
    ...events(),
    ...links(
      link({
        object_id: 'obj_seen',
        attempts: [
          attempt({ object_id: 'obj_seen', object_verified: false }),
          attempt({
            reference: reference('b'),
            kind: 'payment_link',
            object_id: 'obj_checked',
            object_verified: true,
            mode: 'live',
            first_sent_at: '2026-11-27T08:00:00Z',
          }),
          attempt({ reference: reference('c'), kind: 'payment_link', account_id: '', mode: '', first_sent_at: '' }),
        ],
      }),
    ),
  })
  open()
  const attempts = await screen.findByRole('list', { name: 'Attempts at this payment' })
  const [first, second, third] = within(attempts).getAllByRole('listitem')
  expect(screen.getByText(/seen but not accepted blocks a new payment link/)).toBeTruthy()
  expect(first.textContent).toContain('Attempt 1 · Checkout Session')
  expect((within(first).getByRole('textbox', { name: 'Reference' }) as HTMLInputElement).value).toBe(reference('a'))
  expect(within(first).getByRole('textbox', { name: 'Reference' }).hasAttribute('readonly')).toBe(true)
  expect(first.textContent).toContain('Field to search for it: client_reference_id')
  expect(first.textContent).toContain('Object seen: obj_seen (seen, not accepted)')
  expect(first.textContent).toContain('Account acct_shop · Test mode')
  expect(first.textContent).toContain('First sent Nov 26, 2026')
  expect(second.textContent).toContain('Attempt 2 · Payment Link')
  expect((within(second).getByRole('textbox', { name: 'Reference' }) as HTMLInputElement).value).toBe(reference('b'))
  expect(second.textContent).toContain('Field to search for it: metadata[taskto_ref]')
  expect(second.textContent).not.toContain('client_reference_id')
  expect(second.textContent).toContain('Object seen: obj_checked (accepted for this attempt)')
  expect(second.textContent).toContain('Account acct_shop · Live money')
  expect(second.textContent).toContain('First sent Nov 27, 2026')
  // What isn't known is said, not left blank.
  expect(third.textContent).toContain('No object seen')
  expect(third.textContent).toContain('Account not recorded')
  expect(third.textContent).toContain('Never sent')
  // The checklist says where to find them.
  expect(screen.getByText('What to check at Stripe').closest('.ui-notice')?.textContent).toContain(
    'Each payment below lists its attempts with the reference to look for.',
  )
})

test('copying an attempt’s reference puts that attempt’s reference on the clipboard', async () => {
  fakeFetch({
    ...around,
    ...events(),
    ...links(link({ attempts: [attempt(), attempt({ reference: reference('b'), kind: 'payment_link' })] })),
  })
  const writeText = vi.fn(async () => {})
  vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })
  open()
  const attempts = await screen.findByRole('list', { name: 'Attempts at this payment' })
  const [, second] = within(attempts).getAllByRole('listitem')
  fireEvent.click(within(second).getByRole('button', { name: 'Copy reference of attempt 2' }))
  // The button says it copied, for that attempt only.
  expect(await within(second).findByText('Copied')).toBeTruthy()
  expect(within(attempts).getAllByText('Copy')).toHaveLength(1)
  expect(writeText).toHaveBeenCalledTimes(1)
  expect(writeText).toHaveBeenCalledWith(reference('b'))
})

test('a payment with no attempts listed shows no attempts section', async () => {
  fakeFetch({ ...around, ...events(), ...links(link()) })
  open()
  await screen.findByRole('list', { name: 'Payment links to check' })
  expect(screen.queryByRole('list', { name: 'Attempts at this payment' })).toBeNull()
})
