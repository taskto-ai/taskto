import { expect, test } from 'vitest'
import { CHANGE_TOOLS, type Changed, changeOf, turnChanges } from './changes'
import type { ChatMessage } from './transcript'

const json = (v: unknown) => JSON.stringify(v)

const card = { id: 9, board_id: 4, title: 'Call the bank' }

const CASES: [string, Record<string, unknown>, unknown, Changed][] = [
  [
    'write_doc',
    { space_id: 3, path: 'notes/a.md', content: 'x' },
    { commit: 'c1', doc: { path: 'notes/a.md', version: 'v1', size: 1, versioned: true } },
    { kind: 'doc', space: 3, path: 'notes/a.md', change: 'created' },
  ],
  [
    'write_doc',
    { space_id: 3, path: 'notes/a.md', content: 'x', base_version: 'v1' },
    { commit: 'c2', doc: { path: 'notes/a.md', version: 'v2', size: 1, versioned: true } },
    { kind: 'doc', space: 3, path: 'notes/a.md', change: 'edited' },
  ],
  [
    'move_doc',
    { space_id: 3, from: 'a.md', to: 'b/a.md' },
    { commit: 'c3', doc: { path: 'b/a.md', version: 'v1', size: 1, versioned: true } },
    { kind: 'doc', space: 3, path: 'b/a.md', from: 'a.md', change: 'moved' },
  ],
  [
    'delete_doc',
    { space_id: 3, path: 'a.md', base_version: 'v1' },
    { commit: 'c4' },
    { kind: 'doc', space: 3, path: 'a.md', change: 'deleted' },
  ],
  [
    'restore_doc',
    { space_id: 3, path: 'a.md', commit: 'c1' },
    { commit: 'c5', doc: { path: 'a.md', version: 'v3', size: 1, versioned: true } },
    { kind: 'doc', space: 3, path: 'a.md', change: 'restored' },
  ],
  [
    'create_card',
    { space_id: 3, board_id: 4, title: 'Call the bank' },
    card,
    { kind: 'card', space: 3, board: 4, card: 9, title: 'Call the bank', change: 'created' },
  ],
  [
    'update_card',
    { space_id: 3, board_id: 4, card_id: 9, version: 1 },
    card,
    { kind: 'card', space: 3, board: 4, card: 9, title: 'Call the bank', change: 'edited' },
  ],
  [
    'move_card',
    { space_id: 3, board_id: 4, card_id: 9, version: 1, column: 'done' },
    card,
    { kind: 'card', space: 3, board: 4, card: 9, title: 'Call the bank', change: 'moved' },
  ],
  [
    'archive_card',
    { space_id: 3, board_id: 4, card_id: 9, version: 1 },
    card,
    { kind: 'card', space: 3, board: 4, card: 9, title: 'Call the bank', change: 'archived' },
  ],
  [
    'restore_card',
    { space_id: 3, board_id: 4, card_id: 9, version: 2 },
    card,
    { kind: 'card', space: 3, board: 4, card: 9, title: 'Call the bank', change: 'restored' },
  ],
  [
    'set_blocked',
    { space_id: 3, board_id: 4, card_id: 9, version: 2, reason: 'waiting' },
    card,
    { kind: 'card', space: 3, board: 4, card: 9, title: 'Call the bank', change: 'edited' },
  ],
  [
    'comment_card',
    { space_id: 3, board_id: 4, card_id: 9, body: 'Done' },
    { id: 70, card_id: 9, body: 'Done' },
    { kind: 'card', space: 3, board: 4, card: 9, title: '', change: 'commented' },
  ],
  [
    'create_board',
    { space_id: 3, name: 'Plans' },
    { id: 4, space_id: 3, name: 'Plans' },
    { kind: 'board', space: 3, board: 4, name: 'Plans', change: 'created' },
  ],
  [
    'update_board',
    { space_id: 3, board_id: 4, version: 1, name: 'Plans 2' },
    { id: 4, space_id: 3, name: 'Plans 2' },
    { kind: 'board', space: 3, board: 4, name: 'Plans 2', change: 'edited' },
  ],
  [
    'create_app',
    { space_id: 3, name: 'Expenses', definition: {} },
    { id: 5, space_id: 3, name: 'Expenses' },
    { kind: 'app', space: 3, app: 5, name: 'Expenses', change: 'created' },
  ],
  [
    'update_app',
    { space_id: 3, app_id: 5, version: 1 },
    { id: 5, space_id: 3, name: 'Expenses' },
    { kind: 'app', space: 3, app: 5, name: 'Expenses', change: 'edited' },
  ],
  [
    'install_app_template',
    { space_id: 3, template_id: 'invoicing' },
    { id: 6, space_id: 3, name: 'Invoicing' },
    { kind: 'app', space: 3, app: 6, name: 'Invoicing', change: 'created' },
  ],
  [
    'duplicate_app',
    { space_id: 3, app_id: 5, destination_space_id: 8 },
    { id: 7, space_id: 8, name: 'Expenses copy' },
    { kind: 'app', space: 8, app: 7, name: 'Expenses copy', change: 'created' },
  ],
  [
    'run_app_action',
    { space_id: 3, app_id: 5, action: 'add' },
    { action: 'add', kind: 'insert', id: 12, revision: 1, changed: true, effects_queued: 0 },
    { kind: 'record', space: 3, app: 5, record: 12, change: 'created' },
  ],
  [
    'run_app_action',
    { space_id: 3, app_id: 5, action: 'pay', id: 12 },
    { action: 'pay', kind: 'update', id: 12, revision: 2, changed: true, effects_queued: 1 },
    { kind: 'record', space: 3, app: 5, record: 12, change: 'edited' },
  ],
  [
    'run_app_action',
    { space_id: 3, app_id: 5, action: 'remove', id: 12 },
    { action: 'remove', kind: 'delete', id: 12, revision: 3, changed: true, effects_queued: 0 },
    { kind: 'record', space: 3, app: 5, record: 12, change: 'deleted' },
  ],
]

test.each(CASES)('%s: arguments and result become what changed', (tool, args, result, want) => {
  expect(changeOf(tool, args, json(result))).toEqual(want)
})

test('every tool that changes something is in the closed list, and no reader is', () => {
  expect([...CHANGE_TOOLS].sort()).toEqual([...new Set(CASES.map((c) => c[0]))].sort())
  for (const name of [
    'search_docs',
    'read_doc',
    'list_docs',
    'show_card',
    'show_app',
    'query_app_rows',
    'browser_click',
  ]) {
    expect(changeOf(name, { space_id: 3 }, json({ id: 1, path: 'a.md' }))).toBeUndefined()
  }
  for (const name of ['__proto__', 'constructor', 'toString', 'hasOwnProperty']) {
    expect(changeOf(name, { space_id: 3 }, json({ id: 1 }))).toBeUndefined()
  }
})

test('a result that is not JSON, not an object, or cut off is ignored without throwing', () => {
  const args = { space_id: 3, board_id: 4, title: 'x' }
  for (const text of ['', 'the document changed', '[]', 'null', '7', '"x"', '{"id": 9, "board_id"']) {
    expect(changeOf('create_card', args, text)).toBeUndefined()
  }
  expect(changeOf('create_card', null, json(card))).toBeUndefined()
  expect(changeOf('create_card', 'x', json(card))).toBeUndefined()
  expect(changeOf('create_card', [], json(card))).toBeUndefined()
})

test('ids and paths that are not what the server sends are ignored', () => {
  expect(changeOf('create_card', { space_id: 3, board_id: 4 }, json({ ...card, id: '9' }))).toBeUndefined()
  expect(changeOf('create_card', { space_id: 3, board_id: 4 }, json({ ...card, id: 0 }))).toBeUndefined()
  expect(changeOf('create_card', { space_id: 3, board_id: 4 }, json({ ...card, id: 1.5 }))).toBeUndefined()
  expect(changeOf('create_card', { space_id: '3', board_id: 4 }, json(card))).toBeUndefined()
  expect(changeOf('create_card', { board_id: 4 }, json(card))).toBeUndefined()
  expect(changeOf('write_doc', { space_id: 3, path: 7 }, json({ commit: 'c' }))).toBeUndefined()
  expect(changeOf('write_doc', { space_id: 3, path: '' }, json({ commit: 'c' }))).toBeUndefined()
  expect(changeOf('delete_doc', { space_id: 3 }, json({ commit: 'c' }))).toBeUndefined()
  expect(changeOf('create_app', { space_id: 3 }, json({ id: 5, name: 'x' }))).toMatchObject({ space: 3, app: 5 })
  expect(changeOf('create_app', {}, json({ id: 5, name: 'x' }))).toBeUndefined()
})

test('an action that changed nothing, was replayed, or navigated lists no record', () => {
  const args = { space_id: 3, app_id: 5, action: 'add' }
  expect(changeOf('run_app_action', args, json({ kind: 'insert', id: 12, changed: false }))).toBeUndefined()
  expect(changeOf('run_app_action', args, json({ action: 'add', replayed: true, changed: false }))).toBeUndefined()
  expect(changeOf('run_app_action', args, json({ kind: 'insert', changed: true }))).toBeUndefined()
  expect(changeOf('run_app_action', args, json({ kind: 'navigate', id: 12, changed: true }))).toBeUndefined()
  expect(changeOf('run_app_action', args, json({ kind: 'other', id: 12, changed: true }))).toBeUndefined()
})

const doc = (path: string, change: Changed['change'], from?: string): Changed => ({
  kind: 'doc',
  space: 3,
  path,
  ...(from ? { from } : {}),
  change,
})
const cardOf = (change: Changed['change'], title = 'Call the bank'): Extract<Changed, { kind: 'card' }> => ({
  kind: 'card',
  space: 3,
  board: 4,
  card: 9,
  title,
  change,
})

function user(key: string): ChatMessage {
  return { key, role: 'user', text: 'go', tools: [] }
}

function agent(key: string, ...changes: (Changed | undefined)[]): ChatMessage {
  return {
    key,
    role: 'assistant',
    text: '',
    tools: changes.map((changed, i) => ({ id: `${key}-${i}`, key: `${key}.${i}`, name: 'tool', changed })),
  }
}

const run = (...messages: ChatMessage[]) => Object.fromEntries(turnChanges(messages))

test("a turn's changes are listed under its last answer, in the order they happened", () => {
  const got = run(
    user('1.0'),
    agent('2.0', doc('a.md', 'created'), undefined),
    agent('3.0', cardOf('created')),
    agent('4.0'),
  )
  expect(got).toEqual({ '4.0': [doc('a.md', 'created'), cardOf('created')] })
})

test('a turn that changed nothing, or only read, has no list', () => {
  expect(run(user('1.0'), agent('2.0', undefined), agent('3.0'))).toEqual({})
  expect(run()).toEqual({})
})

test("one turn's changes don't leak into the next", () => {
  const got = run(
    user('1.0'),
    agent('2.0', doc('a.md', 'created')),
    user('3.0'),
    agent('4.0', cardOf('created')),
    user('5.0'),
    agent('6.0'),
  )
  expect(got).toEqual({ '2.0': [doc('a.md', 'created')], '4.0': [cardOf('created')] })
})

test('a turn that is still running lists what finished so far under its newest answer', () => {
  const got = run(user('1.0'), agent('2.0', doc('a.md', 'created')), agent('live'))
  expect(got).toEqual({ live: [doc('a.md', 'created')] })
})

test('a turn cut short before its first message still lists its changes', () => {
  expect(run(agent('2.0', cardOf('edited')), agent('3.0'))).toEqual({ '3.0': [cardOf('edited')] })
})

test('created then edited stays created', () => {
  expect(run(user('1.0'), agent('2.0', doc('a.md', 'created')), agent('3.0', doc('a.md', 'edited')))).toEqual({
    '3.0': [doc('a.md', 'created')],
  })
  expect(run(user('1.0'), agent('2.0', cardOf('created', 'Old')), agent('3.0', cardOf('edited', 'New')))).toEqual({
    '3.0': [cardOf('created', 'New')],
  })
})

test('edited twice is one edit, and the last change wins', () => {
  expect(run(user('1.0'), agent('2.0', doc('a.md', 'edited'), doc('a.md', 'edited')))).toEqual({
    '2.0': [doc('a.md', 'edited')],
  })
  expect(run(user('1.0'), agent('2.0', cardOf('edited'), cardOf('moved')))).toEqual({ '2.0': [cardOf('moved')] })
  expect(run(user('1.0'), agent('2.0', doc('a.md', 'edited'), doc('a.md', 'deleted')))).toEqual({
    '2.0': [doc('a.md', 'deleted')],
  })
  expect(run(user('1.0'), agent('2.0', doc('a.md', 'deleted'), doc('a.md', 'restored')))).toEqual({
    '2.0': [doc('a.md', 'restored')],
  })
})

test('created then deleted is not listed at all', () => {
  expect(run(user('1.0'), agent('2.0', doc('a.md', 'created')), agent('3.0', doc('a.md', 'deleted')))).toEqual({})
  expect(run(user('1.0'), agent('2.0', doc('a.md', 'created'), cardOf('created'), doc('a.md', 'deleted')))).toEqual({
    '2.0': [cardOf('created')],
  })
})

test('a card made and then archived is still listed, as archived', () => {
  expect(run(user('1.0'), agent('2.0', cardOf('created'), cardOf('archived')))).toEqual({ '2.0': [cardOf('archived')] })
})

test("a comment doesn't hide the card's title or its creation", () => {
  const comment = { ...cardOf('commented'), title: '' }
  expect(run(user('1.0'), agent('2.0', cardOf('created'), comment))).toEqual({ '2.0': [cardOf('created')] })
  expect(run(user('1.0'), agent('2.0', cardOf('edited'), comment))).toEqual({ '2.0': [cardOf('commented')] })
})

test('a move rewrites the path of what the turn already changed', () => {
  expect(run(user('1.0'), agent('2.0', doc('a.md', 'edited')), agent('3.0', doc('b/a.md', 'moved', 'a.md')))).toEqual({
    '3.0': [doc('b/a.md', 'moved')],
  })
  expect(run(user('1.0'), agent('2.0', doc('a.md', 'created'), doc('b/a.md', 'moved', 'a.md')))).toEqual({
    '2.0': [doc('b/a.md', 'created')],
  })
  expect(
    run(user('1.0'), agent('2.0', doc('a.md', 'created'), doc('b/a.md', 'moved', 'a.md'), doc('b/a.md', 'deleted'))),
  ).toEqual({})
  expect(run(user('1.0'), agent('2.0', doc('b/a.md', 'moved', 'a.md'), doc('b/a.md', 'edited')))).toEqual({
    '2.0': [doc('b/a.md', 'edited')],
  })
})

test('a move of a document the turn did not touch is listed at its new path', () => {
  expect(run(user('1.0'), agent('2.0', doc('x.md', 'edited'), doc('b/a.md', 'moved', 'a.md')))).toEqual({
    '2.0': [doc('x.md', 'edited'), doc('b/a.md', 'moved')],
  })
})

test('a move onto a path the turn listed replaces that entry', () => {
  expect(
    run(user('1.0'), agent('2.0', doc('b.md', 'deleted'), doc('a.md', 'edited'), doc('b.md', 'moved', 'a.md'))),
  ).toEqual({
    '2.0': [doc('b.md', 'moved')],
  })
})

test('the same path in two spaces, or two cards, are different things', () => {
  const other: Changed = { ...doc('a.md', 'created'), space: 4 }
  expect(run(user('1.0'), agent('2.0', doc('a.md', 'created'), other))).toEqual({
    '2.0': [doc('a.md', 'created'), other],
  })
  const second = { ...cardOf('created'), card: 10 }
  expect(run(user('1.0'), agent('2.0', cardOf('created'), second))).toEqual({ '2.0': [cardOf('created'), second] })
})

test('a record is named by its app when the turn made or changed the app', () => {
  const app: Changed = { kind: 'app', space: 3, app: 5, name: 'Expenses', change: 'created' }
  const record: Changed = { kind: 'record', space: 3, app: 5, record: 12, change: 'created' }
  const elsewhere: Changed = { ...record, space: 4, record: 13 }
  expect(run(user('1.0'), agent('2.0', app, record, elsewhere))).toEqual({
    '2.0': [app, { ...record, appName: 'Expenses' }, elsewhere],
  })
})
