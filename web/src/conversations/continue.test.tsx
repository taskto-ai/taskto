// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { beginSession, endSession } from '../session'
import { type Call, fakeFetch, fakeStreams } from '../test/fakes'
import { ConversationView } from './ConversationView'

const BASE = '/api/workspaces/1/conversations/7'

const conversation = (archived = false) => ({
  id: 7,
  workspace_id: 1,
  title: 'Talk',
  archived_at: archived ? '2026-11-18T10:00:00.000Z' : null,
  created_at: '2026-11-18T10:00:00.000Z',
  updated_at: '2026-11-18T10:00:00.000Z',
})

const models = {
  providers: [
    {
      provider: 'script',
      name: 'Scripted',
      scope: 'instance',
      models: [
        { id: 's', name: 'S' },
        { id: 't', name: 'T' },
      ],
    },
    { provider: 'cli-claude', name: 'Claude Code', scope: 'cli', models: [{ id: 'default', name: 'Default' }] },
  ],
}

const asked = { id: 1, kind: 'pi.user', model: [{ role: 'user', content: 'Plan the week' }] }
const failed = {
  id: 2,
  kind: 'pi.assistant',
  model: [{ role: 'assistant', content: [], stopReason: 'error', errorMessage: 'rate limit reached' }],
}
const answered = {
  id: 3,
  kind: 'pi.assistant',
  model: [{ role: 'assistant', content: [{ type: 'text', text: 'Done' }] }],
}
const stopped = { id: 4, kind: 'pi.assistant', model: [{ role: 'assistant', content: [], stopReason: 'aborted' }] }

beforeEach(() => {
  window.history.replaceState(null, '', '/w/1/conversations/7')
})
afterEach(() => {
  cleanup()
  endSession()
  vi.unstubAllGlobals()
})

type Extra = Record<string, Parameters<typeof fakeFetch>[0][string]>

async function show(entries: unknown[], extra: Extra = {}, options: { archived?: boolean; running?: boolean } = {}) {
  const calls = fakeFetch({
    [`GET ${BASE}`]: { body: conversation(options.archived) },
    'GET /api/workspaces/1/models': { body: models },
    [`GET ${BASE}/model`]: { body: { provider: 'script', model_id: 's' } },
    'GET /api/me/cli-accounts': { body: { enabled: true, accounts: [] } },
    ...extra,
  })
  const { sources, factory } = fakeStreams()
  render(<ConversationView workspace={1} id={7} backHref="/w/1/conversations" onChanged={() => {}} streams={factory} />)
  await screen.findByRole('heading', { name: 'Talk' })
  await waitFor(() => expect(sources).toHaveLength(1))
  act(() => sources[0].open())
  const docs = options.running ? { 'pi.live': { run: { taskId: 9 } } } : {}
  act(() => sources[0].send({ type: 'value', value: { entries, docs } }))
  await screen.findByLabelText('Model')
  return calls
}

const note = () => document.querySelector('.ui-chat-continue-note')?.textContent
const writes = (calls: Call[]) => calls.filter((c) => c.method !== 'GET').map((c) => `${c.method} ${c.path}`)
const choose = (label: string) => {
  const select = screen.getByLabelText('Continue on…') as HTMLSelectElement
  const option = Array.from(select.options).find((o) => o.textContent === label)
  if (!option) throw new Error(`no option ${label}`)
  fireEvent.change(select, { target: { value: option.value } })
}

test('a failed last reply offers to try again and to continue on another model', async () => {
  await show([asked, failed])
  expect(screen.getByRole('button', { name: 'Try again' })).toBeTruthy()
  const select = screen.getByLabelText('Continue on…') as HTMLSelectElement
  expect(Array.from(select.options).map((o) => o.textContent)).toEqual([
    'Continue on…',
    'Scripted: T',
    'Claude Code: Default (not connected)',
  ])
})

test.each([
  ['the reply was answered', [asked, failed, answered], {}],
  ['the reply was stopped by the person', [asked, stopped], {}],
  ['the agent is working', [asked, failed], { running: true }],
  ['the conversation is archived', [asked, failed], { archived: true }],
])('nothing is offered when %s', async (_name, entries, options) => {
  await show(entries, {}, options)
  expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull()
  expect(screen.queryByLabelText('Continue on…')).toBeNull()
})

test('Try again continues on the same model without switching it', async () => {
  const calls = await show([asked, failed], { [`POST ${BASE}/continue`]: { status: 202, body: { submission_id: 5 } } })
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
  await waitFor(() => expect(writes(calls)).toEqual([`POST ${BASE}/continue`]))
})

test('choosing another model switches to it, then continues', async () => {
  const calls = await show([asked, failed], {
    [`PUT ${BASE}/model`]: (call) => ({ body: call.body }),
    [`POST ${BASE}/continue`]: { status: 202, body: { submission_id: 5 } },
  })
  choose('Scripted: T')
  await waitFor(() => expect(writes(calls)).toEqual([`PUT ${BASE}/model`, `POST ${BASE}/continue`]))
  expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ provider: 'script', model_id: 't' })
  expect(((await screen.findByLabelText('Model')) as HTMLSelectElement).value).toBe(JSON.stringify(['script', 't']))
})

test('a model that is refused is not continued on', async () => {
  const calls = await show([asked, failed], {
    [`PUT ${BASE}/model`]: { status: 400, body: { error: 'unknown model', code: 'model_unknown' } },
    [`POST ${BASE}/continue`]: { status: 202, body: { submission_id: 5 } },
  })
  choose('Scripted: T')
  await waitFor(() => expect(note()).toBe("That model isn't available to you."))
  expect(writes(calls)).toEqual([`PUT ${BASE}/model`])
})

test('a conversation that is busy says so and can be tried again', async () => {
  let busy = true
  const calls = await show([asked, failed], {
    [`POST ${BASE}/continue`]: () =>
      busy
        ? { status: 409, body: { error: 'busy', code: 'conversation_busy' } }
        : { status: 202, body: { submission_id: 5 } },
  })
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
  await waitFor(() => expect(note()).toBe('This conversation is working: try again when it has finished.'))
  busy = false
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
  await waitFor(() => expect(note()).toBeUndefined())
  expect(writes(calls)).toEqual([`POST ${BASE}/continue`, `POST ${BASE}/continue`])
})

test('a second click while the first continue is in flight sends nothing more', async () => {
  let release: () => void = () => {}
  const calls = await show([asked, failed], {
    [`POST ${BASE}/continue`]: () =>
      new Promise((done) => {
        release = () => done({ status: 202, body: { submission_id: 5 } })
      }),
  })
  const button = screen.getByRole('button', { name: 'Try again' })
  fireEvent.click(button)
  fireEvent.click(button)
  fireEvent.click(button)
  await waitFor(() => expect(writes(calls)).toEqual([`POST ${BASE}/continue`]))
  await act(async () => release())
  expect(writes(calls)).toEqual([`POST ${BASE}/continue`])
})

test('a sign-out while the model is being switched sends no continue', async () => {
  beginSession(1)
  let release: () => void = () => {}
  const calls = await show([asked, failed], {
    [`PUT ${BASE}/model`]: (call) =>
      new Promise((done) => {
        release = () => done({ body: call.body })
      }),
    [`POST ${BASE}/continue`]: { status: 202, body: { submission_id: 5 } },
  })
  choose('Scripted: T')
  await waitFor(() => expect(writes(calls)).toEqual([`PUT ${BASE}/model`]))
  endSession()
  beginSession(2)
  await act(async () => release())
  expect(writes(calls)).toEqual([`PUT ${BASE}/model`])
})
