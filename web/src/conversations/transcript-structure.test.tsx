// @vitest-environment jsdom

import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { appPath } from '../apps/route'
import { fakeFetch, fakeStreams } from '../test/fakes'
import { wholeText } from '../test/text'
import { ConversationView } from './ConversationView'
import stream from './testdata/stream.json'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const conversation = {
  id: 7,
  workspace_id: 1,
  title: 'Structure',
  archived_at: null,
  created_at: '2026-11-18T10:00:00.000Z',
  updated_at: '2026-11-18T10:00:00.000Z',
}

async function show(props: { embedApps?: boolean } = {}) {
  fakeFetch({ 'GET /api/workspaces/1/conversations/7': { body: conversation } })
  const { sources, factory } = fakeStreams()
  render(
    <ConversationView
      workspace={1}
      id={7}
      backHref="/w/1/conversations"
      onChanged={() => {}}
      streams={factory}
      {...props}
    />,
  )
  await screen.findByRole('heading', { name: 'Structure' })
  await waitFor(() => expect(sources).toHaveLength(1))
  act(() => sources[0].open())
  return sources[0]
}

const articles = () => screen.getAllByRole('article')

test("the recorded stream is one message from the person and the agent's reply, in order", async () => {
  const es = await show()
  act(() => {
    for (const event of stream) es.send(event)
  })
  const [mine, reply, ...rest] = articles()
  expect(rest).toHaveLength(0)
  expect(within(mine).getByText('You:')).toBeTruthy()
  expect(within(mine).getByText('Hello there, agent')).toBeTruthy()
  expect(within(reply).getByText('Agent:')).toBeTruthy()
  expect(within(reply).getByText('echo: Hello there, agent')).toBeTruthy()
  expect(reply.getAttribute('aria-busy')).toBeNull()
  expect(screen.getByRole('status').textContent).toBe('The agent replied.')
})

const entries = [
  { id: 1, kind: 'pi.user', model: [{ role: 'user', content: 'Plan the **week**' }] },
  {
    id: 2,
    kind: 'pi.assistant',
    model: [
      {
        role: 'assistant',
        content: [
          { type: 'text', text: 'Here is **the plan**' },
          { type: 'toolCall', id: 'c2', name: 'create_app', arguments: {} },
        ],
        stopReason: 'toolUse',
      },
    ],
  },
  {
    id: 3,
    kind: 'pi.tool',
    model: [
      {
        role: 'toolResult',
        toolCallId: 'c2',
        toolName: 'create_app',
        content: [{ type: 'text', text: JSON.stringify({ id: 5, space_id: 2 }) }],
        isError: false,
      },
    ],
  },
  {
    id: 4,
    kind: 'pi.assistant',
    model: [
      {
        role: 'assistant',
        content: [{ type: 'toolCall', id: 'c4', name: 'cli_turn', arguments: { cli: 'claude' } }],
        stopReason: 'toolUse',
      },
    ],
  },
  {
    id: 5,
    kind: 'pi.tool',
    model: [
      {
        role: 'toolResult',
        toolCallId: 'c4',
        toolName: 'cli_turn',
        content: [{ type: 'text', text: 'done' }],
        details: { transcript: '$ go test\n<b>ok</b>' },
        isError: false,
      },
    ],
  },
  {
    id: 6,
    kind: 'pi.assistant',
    model: [{ role: 'assistant', content: [], stopReason: 'error', errorMessage: 'rate limited' }],
  },
  { id: 7, kind: 'pi.user', model: [{ role: 'user', content: 'And now?' }] },
]
const live = {
  run: { taskId: 9 },
  generation: { message: { role: 'assistant', content: [{ type: 'text', text: '' }], stopReason: 'pending' } },
}

test('every kind of entry shows as its own part, in order', async () => {
  const es = await show({ embedApps: false })
  act(() => es.send({ type: 'value', value: { entries, docs: { 'pi.live': live } } }))
  const list = articles()
  expect(list.map((a) => a.getAttribute('data-role'))).toEqual([
    'user',
    'assistant',
    'assistant',
    'assistant',
    'user',
    'assistant',
  ])
  const [mine, plan, cli, failed, again, pending] = list

  expect(within(mine).getByText('Plan the **week**')).toBeTruthy()

  expect(within(plan).getByText('the plan').tagName).toBe('STRONG')
  expect(within(plan).getByText(wholeText('Used create_app'))).toBeTruthy()
  expect(within(plan).getByText('create_app').tagName).toBe('CODE')
  expect(within(plan).getByRole('link', { name: 'Open the app' }).getAttribute('href')).toBe(appPath(1, 2, 5))

  expect(within(cli).getByText('Claude Code worked')).toBeTruthy()
  expect(within(cli).getByText(/<b>ok<\/b>/).tagName).toBe('PRE')
  expect(cli.querySelector('b')).toBeNull()

  expect(within(failed).getByText('rate limited')).toBeTruthy()

  expect(within(again).getByText('And now?')).toBeTruthy()

  expect(pending.getAttribute('aria-busy')).toBe('true')
  expect(within(pending).getByText('…')).toBeTruthy()
  expect(screen.getByRole('status').textContent).toBe('The agent is working.')
  expect(screen.queryByText('The agent is working…')).toBeNull()
})
