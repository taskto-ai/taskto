import { expect, test } from 'vitest'
import { actionOf } from './explorer'

const link = { kind: 'create_payment_link', integration: 'payments' }

test('the record editor edits through an update that sets nothing going', () => {
  const actions = {
    add_invoice: { kind: 'insert' },
    replace_link: { kind: 'update', effects: [link] },
    send_invoice: { kind: 'update', effects: [link] },
    edit_invoice: { kind: 'update' },
  }
  expect(actionOf(actions, 'update')).toBe('edit_invoice')
  expect(actionOf({ ...actions, edit_invoice: { kind: 'update', effects: [] } }, 'update')).toBe('edit_invoice')
})

test('an app whose every update has effects is edited through the first', () => {
  expect(actionOf({ send: { kind: 'update', effects: [link] }, add: { kind: 'insert' } }, 'update')).toBe('send')
  expect(actionOf({ add: { kind: 'insert' } }, 'update')).toBeUndefined()
  expect(actionOf(undefined, 'delete')).toBeUndefined()
  expect(actionOf({ remove: { kind: 'delete' } }, 'delete')).toBe('remove')
})
