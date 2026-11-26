import { expect, test } from 'vitest'
import type { AppDraft } from '../api'
import { effectLines } from './effectLines'

const f = { list: (items: readonly string[]) => items.join(', '), number: (n: number) => String(n) }

test('a payment link is said with its slot, amount, currency and where the link goes', () => {
  const draft: AppDraft = {
    definition_version: 3,
    requires: ['effect.create_payment_link'],
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
      resend: {
        kind: 'update',
        effects: [
          {
            kind: 'create_payment_link',
            integration: 'payments',
            amount_field: 'total',
            currency_field: 'currency',
            url_field: 'pay_url',
            id_field: 'payment_id',
            when: { field: 'status', op: 'eq', value: 'overdue' },
          },
        ],
      },
    },
  }
  expect(effectLines({ effect_targets: [] }, draft, f)).toEqual([
    'Asks the “payments” integration for a link to pay total in the currency of currency, and puts the link in pay_url and its id in payment_id, only when status is “overdue”.',
    'Asks the “payments” integration for a link to pay total in EUR, and puts the link in pay_url and its id in payment_id.',
  ])
})
