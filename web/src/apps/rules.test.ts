import { expect, test } from 'vitest'
import shared from '../../../internal/apps/testdata/rules.json'
import { holds, type Rule } from './rules'

// The server evaluates the same cases (internal/apps/rules_test.go): the browser hides what the
// server then requires, so the two must agree.
for (const c of shared.cases) {
  test(`rule: ${c.name}`, () => {
    expect(holds(c.rule as Rule, c.row)).toBe(c.holds)
  })
}

test('a malformed rule holds of nothing', () => {
  expect(holds({} as Rule, { a: 1 })).toBe(false)
  expect(holds({ field: 'a', op: 'matches' as never, value: 1 }, { a: 1 })).toBe(false)
  expect(holds({ field: 'a', op: 'eq', value: 1 }, { a: 'x' })).toBe(false)
})
