import { expect, test } from 'vitest'
import shared from '../../../internal/apps/testdata/dates.json'
import { isCalendarDate, isInMonth } from './dates'
import { parseField } from './local'

for (const c of shared.cases) {
  test(`date ${JSON.stringify(c.date)} is ${c.valid ? 'accepted' : 'refused'}, as the server says`, () => {
    expect(isCalendarDate(c.date)).toBe(c.valid)
    const field = { name: 'on', type: 'date' } as Parameters<typeof parseField>[0]
    if (c.valid) expect(parseField(field, c.date)).toBe(c.date)
    else if (c.date !== '') expect(() => parseField(field, c.date)).toThrow()
  })
}

test('a value that is not text is no date', () => {
  expect(isCalendarDate(undefined)).toBe(false)
  expect(isCalendarDate(null)).toBe(false)
  expect(isCalendarDate(20260105)).toBe(false)
})

test('a date groups into its month only when it is a real date', () => {
  expect(isInMonth('2026-04-30', '2026-04')).toBe(true)
  expect(isInMonth('2026-04-31', '2026-04')).toBe(false)
  expect(isInMonth('2026-02-30', '2026-02')).toBe(false)
  expect(isInMonth('2026-03-01', '2026-02')).toBe(false)
})
