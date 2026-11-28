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
  role: 'owner',
  owner: { id: 1, name: 'Ana' },
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

const continuable = {
  providers: [
    models.providers[0],
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

let stream: { send: (m: unknown) => void } | undefined

async function show(entries: unknown[], extra: Extra = {}, options: { archived?: boolean; running?: boolean } = {}) {
  const calls = fakeFetch({
    [`GET ${BASE}`]: { body: conversation(options.archived) },
    'GET /api/workspaces/1/models': { body: models },
    [`GET ${BASE}/model`]: { body: { provider: 'script', model_id: 's' } },
    'GET /api/me/cli-accounts': { body: { enabled: true, accounts: [] } },
    [`GET ${BASE}/continue-models`]: { body: continuable },
    ...extra,
  })
  const { sources, factory } = fakeStreams()
  render(<ConversationView workspace={1} id={7} backHref="/w/1/conversations" onChanged={() => {}} streams={factory} />)
  await screen.findByRole('heading', { name: 'Talk' })
  await waitFor(() => expect(sources).toHaveLength(1))
  stream = sources[0]
  act(() => sources[0].open())
  const docs = options.running ? { 'pi.live': { run: { taskId: 9 } } } : {}
  act(() => sources[0].send({ type: 'value', value: { entries, docs } }))
  await screen.findByLabelText('Model')
  return calls
}

const note = () => document.querySelector('.ui-chat-continue-note')?.textContent
const writes = (calls: Call[]) => calls.filter((c) => c.method !== 'GET').map((c) => `${c.method} ${c.path}`)
const continues = (calls: Call[]) => calls.filter((c) => c.method === 'POST').map((c) => c.body)
const choose = (label: string) => {
  const select = screen.getByLabelText('Continue on…') as HTMLSelectElement
  const option = Array.from(select.options).find((o) => o.textContent === label)
  if (!option) throw new Error(`no option ${label}`)
  fireEvent.change(select, { target: { value: option.value } })
}
const accepted = (call: Call) => ({
  status: 202,
  body: { submission_id: 5, model: (call.body as { model: unknown }).model },
})

test('a failed last reply offers to try again and to continue on another model', async () => {
  await show([asked, failed])
  expect(screen.getByRole('button', { name: 'Try again' })).toBeTruthy()
  const select = screen.getByLabelText('Continue on…') as HTMLSelectElement
  await waitFor(() =>
    expect(Array.from(select.options).map((o) => o.textContent)).toEqual([
      'Continue on…',
      'Scripted: T',
      'Claude Code: Default',
    ]),
  )
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

test('a person it is shared with to read is offered no way to continue a failed reply', async () => {
  const calls = fakeFetch({
    [`GET ${BASE}`]: { body: { ...conversation(), role: 'viewer', owner: { id: 5, name: 'Eve' } } },
    'GET /api/workspaces/1/models': { body: models },
    [`GET ${BASE}/model`]: { body: { provider: 'script', model_id: 's' } },
    [`GET ${BASE}/continue-models`]: { body: continuable },
  })
  const { sources, factory } = fakeStreams()
  render(<ConversationView workspace={1} id={7} backHref="/w/1/conversations" onChanged={() => {}} streams={factory} />)
  await screen.findByRole('heading', { name: 'Talk' })
  await waitFor(() => expect(sources).toHaveLength(1))
  act(() => sources[0].open())
  act(() => sources[0].send({ type: 'value', value: { entries: [asked, failed], docs: {} } }))
  await screen.findByText(/rate limit reached/)
  expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull()
  expect(screen.queryByLabelText('Continue on…')).toBeNull()
  expect(screen.queryByLabelText('Model')).toBeNull()
  expect(calls.filter((c) => c.path.endsWith('/continue-models'))).toEqual([])
  expect(writes(calls)).toEqual([])
})

test('Try again continues on the same model, tied to the failure it answers', async () => {
  const calls = await show([asked, failed], { [`POST ${BASE}/continue`]: accepted })
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
  await waitFor(() => expect(writes(calls)).toEqual([`POST ${BASE}/continue`]))
  expect(continues(calls)).toEqual([{ model: { provider: 'script', model_id: 's' }, after_entry: 2 }])
})

test('choosing another model sends it with the continue, in one request', async () => {
  const calls = await show([asked, failed], { [`POST ${BASE}/continue`]: accepted })
  choose('Scripted: T')
  await waitFor(() => expect(writes(calls)).toEqual([`POST ${BASE}/continue`]))
  expect(continues(calls)).toEqual([{ model: { provider: 'script', model_id: 't' }, after_entry: 2 }])
  expect(((await screen.findByLabelText('Model')) as HTMLSelectElement).value).toBe(JSON.stringify(['script', 't']))
})

test('a model the server refuses is not shown as chosen, and the list is read again', async () => {
  const calls = await show([asked, failed], {
    [`POST ${BASE}/continue`]: { status: 409, body: { error: 'no', code: 'model_unavailable' } },
  })
  const listings = () => calls.filter((c) => c.path === `${BASE}/continue-models`).length
  expect(listings()).toBe(1)
  choose('Claude Code: Default')
  await waitFor(() => expect(note()).toBe("That model can't be used right now: connect its account or choose another."))
  expect(((await screen.findByLabelText('Model')) as HTMLSelectElement).value).toBe(JSON.stringify(['script', 's']))
  await waitFor(() => expect(listings()).toBe(2))
})

test('a continue from a failure the conversation has moved past says so and can be tried again', async () => {
  let moved = true
  const calls = await show([asked, failed], {
    [`POST ${BASE}/continue`]: (call) =>
      moved ? { status: 409, body: { error: 'moved', code: 'conversation_moved' } } : accepted(call),
  })
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
  await waitFor(() =>
    expect(note()).toBe('This conversation changed while you were choosing: check its latest reply and try again.'),
  )
  moved = false
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
  await waitFor(() => expect(note()).toBeUndefined())
  expect(writes(calls)).toEqual([`POST ${BASE}/continue`, `POST ${BASE}/continue`])
})

test('a conversation that is busy says so and can be tried again', async () => {
  let busy = true
  const calls = await show([asked, failed], {
    [`POST ${BASE}/continue`]: (call) =>
      busy ? { status: 409, body: { error: 'busy', code: 'conversation_busy' } } : accepted(call),
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
    [`POST ${BASE}/continue`]: (call) =>
      new Promise((done) => {
        release = () => done(accepted(call))
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

test('after the server accepts a continue the buttons stay disabled until the stream shows the new run', async () => {
  const calls = await show([asked, failed], { [`POST ${BASE}/continue`]: accepted })
  const button = screen.getByRole('button', { name: 'Try again' }) as HTMLButtonElement
  fireEvent.click(button)
  await waitFor(() => expect(writes(calls)).toEqual([`POST ${BASE}/continue`]))
  await act(async () => {})
  expect(button.disabled).toBe(true)
  expect((screen.getByLabelText('Continue on…') as HTMLSelectElement).disabled).toBe(true)
  fireEvent.click(button)
  expect(writes(calls)).toEqual([`POST ${BASE}/continue`])
})

test('a new failure after the continue offers the buttons again, tied to the new failure', async () => {
  const calls = await show([asked, failed], { [`POST ${BASE}/continue`]: accepted })
  fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
  await waitFor(() => expect(writes(calls)).toEqual([`POST ${BASE}/continue`]))
  const again = { ...failed, id: 5 }
  act(() => stream?.send({ type: 'value', value: { entries: [asked, failed, again], docs: {} } }))
  const button = await screen.findByRole('button', { name: 'Try again' })
  await waitFor(() => expect((button as HTMLButtonElement).disabled).toBe(false))
  fireEvent.click(button)
  await waitFor(() => expect(continues(calls).at(-1)).toMatchObject({ after_entry: 5 }))
})

test('a sign-out while the continue is in flight does not mark the model as chosen', async () => {
  beginSession(1)
  let release: () => void = () => {}
  const calls = await show([asked, failed], {
    [`POST ${BASE}/continue`]: (call) =>
      new Promise((done) => {
        release = () => done(accepted(call))
      }),
  })
  choose('Scripted: T')
  await waitFor(() => expect(writes(calls)).toEqual([`POST ${BASE}/continue`]))
  endSession()
  beginSession(2)
  await act(async () => release())
  expect(((await screen.findByLabelText('Model')) as HTMLSelectElement).value).toBe(JSON.stringify(['script', 's']))
})

test('the model picker is disabled while a continue is in flight, so its model is the one shown', async () => {
  let release: () => void = () => {}
  const calls = await show([asked, failed], {
    [`POST ${BASE}/continue`]: (call) =>
      new Promise((done) => {
        release = () => done(accepted(call))
      }),
  })
  const picker = (await screen.findByLabelText('Model')) as HTMLSelectElement
  expect(picker.disabled).toBe(false)
  choose('Scripted: T')
  await waitFor(() => expect(writes(calls)).toEqual([`POST ${BASE}/continue`]))
  expect(picker.disabled).toBe(true)
  await act(async () => release())
  await waitFor(() => expect(picker.disabled).toBe(false))
  expect(picker.value).toBe(JSON.stringify(['script', 't']))
})

test('after a model chosen in the picker, a continue on another model is what the picker shows', async () => {
  const calls = await show([asked, failed], {
    [`PUT ${BASE}/model`]: (call) => ({ body: call.body }),
    [`POST ${BASE}/continue`]: accepted,
  })
  const picker = (await screen.findByLabelText('Model')) as HTMLSelectElement
  fireEvent.change(picker, { target: { value: JSON.stringify(['script', 't']) } })
  await waitFor(() => expect(writes(calls)).toEqual([`PUT ${BASE}/model`]))
  await waitFor(() => expect(picker.disabled).toBe(false))
  expect(picker.value).toBe(JSON.stringify(['script', 't']))
  choose('Scripted: S')
  await waitFor(() =>
    expect(continues(calls)).toEqual([{ model: { provider: 'script', model_id: 's' }, after_entry: 2 }]),
  )
  await waitFor(() => expect(picker.value).toBe(JSON.stringify(['script', 's'])))
})

test('the picker shows the model the server says the run was admitted on, not the one that was sent', async () => {
  await show([asked, failed], {
    [`POST ${BASE}/continue`]: {
      status: 202,
      body: { submission_id: 5, model: { provider: 'script', model_id: 's' } },
    },
  })
  choose('Scripted: T')
  const picker = (await screen.findByLabelText('Model')) as HTMLSelectElement
  await waitFor(() => expect(picker.disabled).toBe(false))
  expect(picker.value).toBe(JSON.stringify(['script', 's']))
})

const privateNote = { id: 3, kind: 'taskto.note', data: { v: 1, author: 1, text: 'for the record' } }

test('a private note after a failed reply keeps the failure offered and ties the continue to the failed reply', async () => {
  const calls = await show([asked, failed, privateNote], { [`POST ${BASE}/continue`]: accepted })
  fireEvent.click(await screen.findByRole('button', { name: 'Try again' }))
  await waitFor(() => expect(writes(calls)).toEqual([`POST ${BASE}/continue`]))
  expect(continues(calls)).toEqual([{ model: { provider: 'script', model_id: 's' }, after_entry: 2 }])
})

test('a note added after the buttons are shown does not take them away or move the failure they answer', async () => {
  const calls = await show([asked, failed], { [`POST ${BASE}/continue`]: accepted })
  act(() => stream?.send({ type: 'value', value: { entries: [asked, failed, privateNote], docs: {} } }))
  fireEvent.click(await screen.findByRole('button', { name: 'Try again' }))
  await waitFor(() =>
    expect(continues(calls)).toEqual([{ model: { provider: 'script', model_id: 's' }, after_entry: 2 }]),
  )
})

test('a picker response that arrives after a later continue does not replace the model the continue chose', async () => {
  let releasePut: () => void = () => {}
  const calls = await show([asked, failed], {
    [`PUT ${BASE}/model`]: (call) =>
      new Promise((done) => {
        releasePut = () => done({ body: call.body })
      }),
    [`POST ${BASE}/continue`]: accepted,
  })
  const picker = (await screen.findByLabelText('Model')) as HTMLSelectElement
  fireEvent.change(picker, { target: { value: JSON.stringify(['script', 't']) } })
  await waitFor(() => expect(writes(calls)).toEqual([`PUT ${BASE}/model`]))
  choose('Claude Code: Default')
  await waitFor(() => expect(writes(calls)).toEqual([`PUT ${BASE}/model`, `POST ${BASE}/continue`]))
  await act(async () => releasePut())
  await waitFor(() => expect(picker.disabled).toBe(false))
  expect(picker.value).toBe(JSON.stringify(['cli-claude', 'default']))
})
