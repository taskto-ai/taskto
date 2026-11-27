import { applyImmutable, type Op } from '@earendil-works/chord/delta'
import { expect, test } from 'vitest'
import { turnChanges } from './changes'
import stream from './testdata/stream.json'
import turn from './testdata/turn-changes.json'
import { type ConversationView, transcript } from './transcript'

type Event = { type: string; value?: ConversationView; ops?: Op[] }

function views(): ConversationView[] {
  const out: ConversationView[] = []
  let view: ConversationView | undefined
  for (const event of stream as Event[]) {
    if (event.type === 'value') view = event.value
    else if (event.type === 'ops' && view) view = applyImmutable(view, event.ops ?? [])
    else continue
    if (view) out.push(view)
  }
  return out
}

test('follows a message and its reply through the stream', () => {
  const steps = views().map(transcript)
  expect(steps[0]).toEqual({ messages: [], running: false, truncated: false })

  expect(steps[1].running).toBe(true)
  expect(steps[1].messages.map((m) => [m.role, m.text])).toEqual([['user', 'Hello there, agent']])

  expect(steps[2].messages).toHaveLength(1)

  expect(steps[4].messages.at(-1)).toMatchObject({ role: 'assistant', text: '', pending: true })
  expect(steps[5].messages.at(-1)).toMatchObject({ role: 'assistant', text: 'echo: Hello', pending: true })

  const done = steps[6]
  expect(done.running).toBe(false)
  expect(done.messages.map((m) => [m.role, m.text, m.pending ?? false])).toEqual([
    ['user', 'Hello there, agent', false],
    ['assistant', 'echo: Hello there, agent', false],
  ])
})

test('shows tool calls, failures, and that earlier entries are left out', () => {
  const t = transcript({
    before: 3,
    entries: [
      {
        id: 4,
        kind: 'pi.assistant',
        model: [
          { role: 'assistant', content: [{ type: 'toolCall', id: 'c1', name: 'search_docs', arguments: {} }] },
          { role: 'toolResult', content: [{ type: 'text', text: 'secret result' }] },
        ],
      },
      {
        id: 5,
        kind: 'pi.assistant',
        model: [{ role: 'assistant', content: [], stopReason: 'error', errorMessage: 'rate limited' }],
      },
      { id: 6, kind: 'something.new', model: [{ role: 'user', content: [{ type: 'image', data: '…' }] }] },
    ],
  })
  expect(t.truncated).toBe(true)
  expect(t.messages).toEqual([
    { key: '4.0', role: 'assistant', text: '', tools: [{ id: 'c1', key: '4.0.0', name: 'search_docs' }] },
    { key: '5.0', role: 'assistant', text: '', tools: [], error: 'rate limited' },
  ])
})

test('passes over what it does not know', () => {
  expect(transcript(null).messages).toEqual([])
  expect(transcript({ entries: [{ id: 1, kind: 'x' }], docs: { 'pi.live': 'odd' } }).messages).toEqual([])
})

test('a tool use names the app only from a saved result, never from its arguments', () => {
  const call = (id: string, name: string, args: unknown = { space_id: 9, app_id: 9 }) => ({
    type: 'toolCall',
    id,
    name,
    arguments: args,
  })
  const result = (toolCallId: string, toolName: string, text: string, isError = false) => ({
    role: 'toolResult',
    toolCallId,
    toolName,
    content: [{ type: 'text', text }],
    isError,
  })
  const saved = JSON.stringify({ id: 1, space_id: 2, name: 'Expenses', version: 1 })
  const t = transcript({
    entries: [
      {
        id: 1,
        kind: 'pi.assistant',
        model: [
          {
            role: 'assistant',
            content: [
              call('ok', 'create_app'),
              call('refused', 'create_app'),
              call('odd', 'update_app'),
              call('other', 'show_app'),
            ],
          },
          result('refused', 'create_app', 'error: the definition has no root', true),
          result('odd', 'update_app', 'not JSON'),
          result('other', 'show_app', saved),
        ],
      },
      { id: 2, kind: 'pi.tool', model: [result('ok', 'create_app', saved)] },
    ],
  })
  expect(t.messages[0].tools).toEqual([
    {
      id: 'ok',
      key: '1.0.0',
      name: 'create_app',
      app: { space: 2, app: 1 },
      changed: { kind: 'app', space: 2, app: 1, name: 'Expenses', change: 'created' },
    },
    { id: 'refused', key: '1.0.1', name: 'create_app' },
    { id: 'odd', key: '1.0.2', name: 'update_app' },
    { id: 'other', key: '1.0.3', name: 'show_app' },
  ])
})

test('a result answers the latest call with its id: ids can repeat across turns', () => {
  const call = (name: string) => ({ type: 'toolCall', id: 'call_0', name, arguments: {} })
  const result = (name: string, app: number) => ({
    role: 'toolResult',
    toolCallId: 'call_0',
    toolName: name,
    content: [{ type: 'text', text: JSON.stringify({ id: app, space_id: 2, name: 'x', version: 1 }) }],
  })
  const t = transcript({
    entries: [
      { id: 1, kind: 'pi.assistant', model: [{ role: 'assistant', content: [call('create_app')] }] },
      { id: 2, kind: 'pi.tool', model: [result('create_app', 1)] },
      { id: 3, kind: 'pi.assistant', model: [{ role: 'assistant', content: [call('update_app')] }] },
      { id: 4, kind: 'pi.tool', model: [result('update_app', 5)] },
    ],
  })
  expect(t.messages.map((m) => m.tools)).toEqual([
    [
      {
        id: 'call_0',
        key: '1.0.0',
        name: 'create_app',
        app: { space: 2, app: 1 },
        changed: { kind: 'app', space: 2, app: 1, name: 'x', change: 'created' },
      },
    ],
    [
      {
        id: 'call_0',
        key: '3.0.0',
        name: 'update_app',
        app: { space: 2, app: 5 },
        changed: { kind: 'app', space: 2, app: 5, name: 'x', change: 'edited' },
      },
    ],
  ])
})

test('a call is named by where it is, so a repeated id is still another call', () => {
  const call = { role: 'assistant', content: [{ type: 'toolCall', id: 'call_0', name: 'create_app', arguments: {} }] }
  const t = transcript({
    entries: [
      { id: 1, kind: 'pi.assistant', model: [call] },
      { id: 3, kind: 'pi.assistant', model: [call] },
    ],
  })
  const keys = t.messages.flatMap((m) => m.tools.map((u) => u.key))
  expect(keys).toEqual(['1.0.0', '3.0.0'])
})

test('an app a CLI turn saved is the app of its cli_turn call, from the result it kept', () => {
  const t = transcript({
    entries: [
      {
        id: 1,
        kind: 'pi.assistant',
        model: [
          {
            role: 'assistant',
            content: [{ type: 'toolCall', id: 'c', name: 'cli_turn', arguments: { cli: 'claude' } }],
          },
          {
            role: 'toolResult',
            toolCallId: 'c',
            toolName: 'cli_turn',
            content: [{ type: 'text', text: 'Saved.' }],
            details: {
              transcript: '▸ mcp__taskto__create_app',
              apps: [{ space: 2, app: 1 }, { space: 'x', app: 3 }, { space: 2, app: 4 }, null],
            },
          },
        ],
      },
    ],
  })
  expect(t.messages[0].tools[0].app).toEqual({ space: 2, app: 4 })
  const none = transcript({
    entries: [
      {
        id: 1,
        kind: 'pi.assistant',
        model: [
          {
            role: 'assistant',
            content: [{ type: 'toolCall', id: 'c', name: 'cli_turn', arguments: { cli: 'claude' } }],
          },
          { role: 'toolResult', toolCallId: 'c', toolName: 'cli_turn', content: [], details: { apps: 'nope' } },
        ],
      },
    ],
  })
  expect(none.messages[0].tools[0].app).toBeUndefined()
})

test('keeps what each call that changed something changed, and nothing for readers or failed calls', () => {
  const t = transcript({ entries: turn })
  const used = t.messages.flatMap((m) => m.tools).map((u) => [u.name, u.changed])
  expect(used).toEqual([
    ['list_docs', undefined],
    ['write_doc', { kind: 'doc', space: 3, path: 'notes/a.md', change: 'created' }],
    ['create_card', { kind: 'card', space: 3, board: 4, card: 9, title: 'Call the bank', change: 'created' }],
    ['write_doc', undefined],
    ['run_app_action', { kind: 'record', space: 2, app: 5, record: 12, change: 'created' }],
  ])
  expect([...turnChanges(t.messages)]).toEqual([
    [
      '9.0',
      [
        { kind: 'doc', space: 3, path: 'notes/a.md', change: 'created' },
        { kind: 'card', space: 3, board: 4, card: 9, title: 'Call the bank', change: 'created' },
        { kind: 'record', space: 2, app: 5, record: 12, change: 'created' },
      ],
    ],
  ])
})

test('a result that arrives for a call of another name is not taken for a change', () => {
  const t = transcript({
    entries: [
      {
        id: 1,
        kind: 'pi.assistant',
        model: [
          {
            role: 'assistant',
            content: [{ type: 'toolCall', id: 'x', name: 'read_doc', arguments: { space_id: 3, path: 'a.md' } }],
          },
          {
            role: 'toolResult',
            toolCallId: 'x',
            toolName: 'write_doc',
            content: [{ type: 'text', text: '{"commit":"c"}' }],
          },
        ],
      },
    ],
  })
  expect(t.messages.flatMap((m) => m.tools).map((u) => u.changed)).toEqual([undefined])
})
