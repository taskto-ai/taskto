import { expect, test } from 'vitest'
import type { Card, Comment } from '../api'
import { withComments } from './comments'
import { dueState } from './due'
import { boardFilter } from './filters'
import { dropPosition } from './move'

const card = (id: number, column: Card['column'], extra: Partial<Card> = {}) =>
  ({ id, column, title: `Card ${id}`, labels: [], due: null, ...extra }) as Card

test('a dropped card goes where it lands, counted among the column without it', () => {
  const cards = [card(1, 'todo'), card(2, 'todo'), card(3, 'todo'), card(4, 'doing')]
  expect(dropPosition(cards, cards[0], 'todo', cards[2])).toBe(1)
  expect(dropPosition(cards, cards[0], 'todo')).toBe(2)
  expect(dropPosition(cards, cards[0], 'doing', cards[3])).toBe(0)
  expect(dropPosition(cards, cards[0], 'doing')).toBe(1)
  expect(dropPosition(cards, cards[3], 'review')).toBe(0)
})

test('a drop that leaves the card where it is moves nothing', () => {
  const cards = [card(1, 'todo'), card(2, 'todo'), card(3, 'todo')]
  expect(dropPosition(cards, cards[1], 'todo', cards[1])).toBeNull()
  expect(dropPosition(cards, cards[1], 'todo', cards[2])).toBeNull()
  expect(dropPosition(cards, cards[2], 'todo')).toBeNull()
})

test('the filters narrow to a label and to the overdue cards, and keep a label no card has', () => {
  const today = '2026-03-10'
  const cards = [
    card(1, 'todo', { labels: ['ux', 'bug'], due: '2026-03-01' }),
    card(2, 'done', { labels: ['bug'], due: '2026-03-01' }),
    card(3, 'doing', { due: '2026-04-01' }),
  ]
  const all = boardFilter(cards, '', false, today)
  expect(all).toMatchObject({ allLabels: ['bug', 'ux'], filtering: false, filterable: true })
  expect(cards.filter(all.shown).map((c) => c.id)).toEqual([1, 2, 3])

  const bug = boardFilter(cards, 'bug', false, today)
  expect(bug.filtering).toBe(true)
  expect(cards.filter(bug.shown).map((c) => c.id)).toEqual([1, 2])

  const late = boardFilter(cards, '', true, today)
  expect(cards.filter(late.shown).map((c) => c.id)).toEqual([1])

  const gone = boardFilter(cards, 'old', false, today)
  expect(gone.allLabels).toEqual(['bug', 'old', 'ux'])
  expect(cards.filter(gone.shown)).toEqual([])
})

test('a board with no labels or due dates has no filters, unless one is on', () => {
  const cards = [card(1, 'todo')]
  expect(boardFilter(cards, '', false, '2026-03-10').filterable).toBe(false)
  expect(boardFilter(cards, '', true, '2026-03-10').filterable).toBe(true)
})

const comment = (id: number, body = `comment ${id}`): Comment => ({
  id,
  card_id: 1,
  author: null,
  body,
  created_at: '2026-01-01T00:00:00Z',
  mentions: [],
})

test('a newer read adds its comments in order, replaces changed ones and keeps those held only here', () => {
  const have = [comment(1), comment(3)]
  const merged = withComments(have, [comment(1, 'edited'), comment(2)])
  expect(merged.map((c) => [c.id, c.body])).toEqual([
    [1, 'edited'],
    [2, 'comment 2'],
    [3, 'comment 3'],
  ])
})

test("a read that renames only a comment's mention replaces the comment", () => {
  const held = { ...comment(1), mentions: [{ id: 5, name: 'Old name' }] }
  const have = [held]
  const merged = withComments(have, [{ ...held, mentions: [{ id: 5, name: 'New name' }] }])
  expect(merged).not.toBe(have)
  expect(merged[0].mentions).toEqual([{ id: 5, name: 'New name' }])
})

test('a read with nothing new gives the same thread back', () => {
  const have = [comment(1), comment(2)]
  expect(withComments(have, [comment(1), comment(2)])).toBe(have)
  expect(withComments(have, [])).toBe(have)
})

test('a due date is overdue before today, soon today and tomorrow, and later after that; never for a finished card', () => {
  const today = '2026-03-10'
  const due = (d: string, column: Card['column'] = 'todo') => dueState({ due: d, column }, today)
  expect(dueState({ due: null, column: 'todo' }, today)).toBeNull()
  expect(due('2026-03-09')).toBe('overdue')
  expect(due('2026-03-10')).toBe('soon')
  expect(due('2026-03-11')).toBe('soon')
  expect(due('2026-03-12')).toBe('later')
  expect(dueState({ due: '2026-04-01', column: 'todo' }, '2026-03-31')).toBe('soon')
  expect(dueState({ due: '2027-01-01', column: 'todo' }, '2026-12-31')).toBe('soon')
  expect(due('2026-03-09', 'done')).toBe('later')
  expect(due('2026-03-10', 'archived')).toBe('later')
})
