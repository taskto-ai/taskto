import { expect, test } from 'vitest'
import { boardItem, documentItem, parseBoardItem, parseDocumentItem, parseRoute, routePath } from './router'

test('parses routes, and a malformed item is no route rather than a crash', () => {
  expect(parseRoute('/w/1/knowledge/a%20b')).toEqual({ workspace: 1, section: 'knowledge', item: 'a b' })
  expect(parseRoute('/w/1/knowledge/a%zz')).toBeNull()
  expect(parseRoute('/w/1/nowhere')).toBeNull()
  // Where a sign-in sends a new account that joined through an invite.
  expect(parseRoute('/w/7/conversations')).toEqual({ workspace: 7, section: 'conversations' })
  expect(parseRoute('/w/7')).toBeNull()
})

test('links into the boards and the knowledge base carry the ids and the path', () => {
  expect(parseBoardItem(boardItem(7, 12, 88))).toEqual({ space: 7, board: 12, card: 88 })
  expect(parseBoardItem(boardItem(7))).toEqual({ space: 7 })
  expect(parseBoardItem('x')).toBeNull()
  expect(parseBoardItem(undefined)).toBeNull()
  const item = documentItem(9, 'inspections/14.md')
  expect(parseDocumentItem(parseRoute(routePath({ workspace: 5, section: 'knowledge', item }))?.item)).toEqual({
    space: 9,
    path: 'inspections/14.md',
  })
  expect(parseDocumentItem(undefined)).toBeNull()
})
