import { expect, test } from 'vitest'
import { parseRoute } from './router'

test('parses routes, and a malformed item is no route rather than a crash', () => {
  expect(parseRoute('/w/1/knowledge/a%20b')).toEqual({ workspace: 1, section: 'knowledge', item: 'a b' })
  expect(parseRoute('/w/1/knowledge/a%zz')).toBeNull()
  expect(parseRoute('/w/1/nowhere')).toBeNull()
})
