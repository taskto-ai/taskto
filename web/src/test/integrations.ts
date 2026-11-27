import type { Integration, IntegrationApproval, IntegrationContract, SlotStatus } from '../api'

export const contract: IntegrationContract = {
  version: 5,
  kind: 'payments',
  operations: ['create_payment_link'],
  sends: ['customer', 'number', 'total'],
  currencies: ['EUR'],
  events: ['payment.expired', 'payment.succeeded'],
  writes: [
    { field: 'paid_at', values: [], from: ['occurred_at'] },
    { field: 'status', values: ['paid', 'sent'], from: [] },
  ],
  only_if: [{ event: 'payment.succeeded', rule: { field: 'status', op: 'eq', value: 'sent' } }],
  links: [
    {
      action: 'send',
      action_kind: 'update',
      amount_field: 'total',
      describes: ['customer', 'number'],
      describes_id: true,
      currency: 'EUR',
      url_field: 'pay_url',
      id_field: 'payment_id',
    },
  ],
  handlers: [
    { event: 'payment.expired', sets: [{ field: 'status', value: 'sent' }] },
    {
      event: 'payment.succeeded',
      sets: [
        { field: 'paid_at', from: 'occurred_at' },
        { field: 'status', value: 'paid' },
      ],
    },
  ],
  fields: [
    { name: 'customer', type: 'string', required: true, max_length: 80 },
    { name: 'number', type: 'string', required: true, max_length: 20 },
    { name: 'paid_at', type: 'date' },
    { name: 'pay_url', type: 'string', max_length: 2048 },
    { name: 'payment_id', type: 'string', max_length: 255 },
    {
      name: 'receipt',
      type: 'attachment',
      required_when: { field: 'status', op: 'eq', value: 'paid' },
      max_files: 2,
      max_bytes: 5000000,
    },
    { name: 'status', type: 'choice', required: true, options: ['draft', 'paid', 'sent'] },
    { name: 'total', type: 'number', required: true, min: 1, max: 100000 },
  ],
}

export const slot = (over: Partial<SlotStatus> = {}): SlotStatus => ({
  slot: 'payments',
  kind: 'payments',
  contract,
  contract_hash: 'a'.repeat(64),
  status: 'none',
  approval_id: null,
  integration: null,
  live: null,
  ...over,
})

export const approval = (over: Partial<IntegrationApproval> = {}): IntegrationApproval => ({
  id: 4,
  app_id: 9,
  app_name: 'Invoices',
  space_id: 12,
  space_name: 'Shop floor',
  space_kind: 'shared',
  slot: 'payments',
  kind: 'payments',
  contract,
  contract_hash: 'a'.repeat(64),
  status: 'requested',
  live: false,
  integration: null,
  space_grant: 'none',
  requested_by: { id: 2, name: 'Alice', agent: false },
  requested_at: '2026-11-24T10:00:00.000Z',
  decided_by: null,
  decided_at: null,
  ...over,
})

export const shop: Integration = {
  id: 7,
  provider: 'stripe',
  kind: 'payments',
  name: 'shop',
  display_name: 'Shop',
  mode: 'test',
  settings: {},
  max_amount_minor: 500000,
  currencies: ['EUR'],
  secrets: { api_key: 'set', webhook_secret: 'not set' },
  webhook_url: null,
  disabled: false,
  disabled_at: null,
  spaces: [12],
  approval_spaces: [],
  other_spaces: 0,
  created_at: '2026-11-24T10:00:00.000Z',
  updated_at: '2026-11-24T10:00:00.000Z',
  last_event_at: null,
}
