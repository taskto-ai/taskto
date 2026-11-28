import { expect, test } from 'vitest'
import shared from '../../../internal/apps/testdata/spaces.json'
import { parseField } from './local'

type Case = { type: string; input: string; stored?: unknown; refused?: boolean }

for (const c of shared.cases as Case[]) {
  const label = `${c.type} ${JSON.stringify(c.input)}`
  test(`${label} is ${c.refused ? 'refused' : c.stored === null ? 'left out' : 'stored'}, as the server says`, () => {
    const field = { name: 'f', type: c.type } as Parameters<typeof parseField>[0]
    if (c.refused) {
      expect(() => parseField(field, c.input)).toThrow()
      return
    }
    expect(parseField(field, c.input)).toBe(c.stored === null ? undefined : c.stored)
  })
}

test('a long run of spaces inside a text is trimmed in linear time', () => {
  const value = `x${' '.repeat(200_000)}x`
  const field = { name: 'f', type: 'string', max_length: 400_000 } as Parameters<typeof parseField>[0]
  const started = performance.now()
  expect(parseField(field, ` ${value} `)).toBe(value)
  expect(performance.now() - started).toBeLessThan(1000)
})
