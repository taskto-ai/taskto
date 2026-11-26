// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, renderHook, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeAll, beforeEach, expect, test, vi } from 'vitest'
import afterAdd from '../../../internal/apps/testdata/expenses.after-add.json'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { SESSION_LOST } from '../api'
import { parseRoute, usePath } from '../router'
import { beginSession, endSession } from '../session'
import { type Call, fakeFetch, fakeStreams } from '../test/fakes'
import { wholeText } from '../test/text'
import { ConversationsPage } from './ConversationsPage'
import stream from './testdata/stream.json'
import { RETRY_MAX_MS, RETRY_MS, useConversationStream } from './useConversationStream'

const conversation = (id: number, title: string) => ({
  id,
  workspace_id: 1,
  title,
  archived_at: null,
  created_at: '2026-11-18T10:00:00.000Z',
  updated_at: '2026-11-18T10:00:00.000Z',
})

// A saved app's screen is a lazily loaded component, which the first test that shows one loads
// (and vite compiles) on the spot: on a machine with more work than cores that took longer than
// any wait for what the page shows. Load it once here, where the hook has a time of its own, so
// the tests wait only for the page to draw it.
beforeAll(async () => {
  await import('../apps/AppPreview')
}, 60_000)

beforeEach(() => window.history.replaceState(null, '', '/w/1/conversations'))
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

// The page as the shell mounts it, following the URL.
function Page({ streams }: { streams: ReturnType<typeof fakeStreams>['factory'] }) {
  const route = parseRoute(usePath())
  return <ConversationsPage workspace={1} open={route?.item} streams={streams} />
}

test('lists conversations, creates one and opens it', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [conversation(3, 'Plans')] } },
    'POST /api/workspaces/1/conversations': { status: 201, body: conversation(7, '') },
    'GET /api/workspaces/1/conversations/7': { body: conversation(7, '') },
  })
  const { sources, factory } = fakeStreams()
  render(<Page streams={factory} />)

  expect(await screen.findByRole('link', { name: /Plans/ })).toBeTruthy()
  fireEvent.click(screen.getByRole('button', { name: 'New' }))

  await waitFor(() => expect(window.location.pathname).toBe('/w/1/conversations/7'))
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({})
  await waitFor(() => expect(sources.at(-1)?.url).toBe('/api/workspaces/1/conversations/7/events'))
})

test('a conversation made after the page went (the session ended) is not opened', async () => {
  let answer: () => void = () => {}
  fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [] } },
    'POST /api/workspaces/1/conversations': () =>
      new Promise((done) => {
        answer = () => done({ status: 201, body: conversation(7, '') })
      }),
  })
  const { factory } = fakeStreams()
  const { unmount } = render(<Page streams={factory} />)
  fireEvent.click(await screen.findByRole('button', { name: 'New' }))
  unmount()
  window.history.replaceState(null, '', '/')
  await act(async () => answer())
  expect(window.location.pathname).toBe('/')
})

test('a conversation made after the session ended, the page still showing, is not opened', async () => {
  let answer: () => void = () => {}
  fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [] } },
    'POST /api/workspaces/1/conversations': () =>
      new Promise((done) => {
        answer = () => done({ status: 201, body: conversation(7, '') })
      }),
  })
  const { factory } = fakeStreams()
  beginSession(1)
  render(<Page streams={factory} />)
  fireEvent.click(await screen.findByRole('button', { name: 'New' }))
  // Signing out has begun; the shell isn't gone yet.
  endSession()
  await act(async () => answer())
  expect(window.location.pathname).toBe('/w/1/conversations')
})

test('shows the stream as it comes, and sends a message', async () => {
  window.history.replaceState(null, '', '/w/1/conversations/7')
  const calls = fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [conversation(7, 'Hello')] } },
    'GET /api/workspaces/1/conversations/7': { body: conversation(7, 'Hello') },
    'POST /api/workspaces/1/conversations/7/messages': { status: 202, body: { submission_id: 12 } },
  })
  const { sources, factory } = fakeStreams()
  render(<Page streams={factory} />)
  await waitFor(() => expect(sources).toHaveLength(1))
  const es = sources[0]

  act(() => es.open())
  expect(screen.getByText('Live')).toBeTruthy()
  act(() => {
    for (const event of stream.slice(0, 6)) es.send(event)
  })
  expect(screen.getByText('Hello there, agent')).toBeTruthy()
  expect(screen.getByText('echo: Hello')).toBeTruthy()
  act(() => es.send(stream[6]))
  expect(screen.getByText('echo: Hello there, agent')).toBeTruthy()
  expect(screen.queryByText('The agent is working…')).toBeNull()

  const box = screen.getByLabelText('Message')
  fireEvent.change(box, { target: { value: '  What next?  ' } })
  fireEvent.keyDown(box, { key: 'Enter' })
  await waitFor(() => expect((box as HTMLTextAreaElement).value).toBe(''))
  const sent = calls.find((c) => c.method === 'POST')
  expect(sent?.path).toBe('/api/workspaces/1/conversations/7/messages')
  expect(sent?.body).toMatchObject({ content: 'What next?' })
  expect(typeof (sent?.body as { request_id?: unknown } | undefined)?.request_id).toBe('string')
})

test('text typed while a message is sending stays', async () => {
  window.history.replaceState(null, '', '/w/1/conversations/7')
  fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [conversation(7, 'Hello')] } },
    'GET /api/workspaces/1/conversations/7': { body: conversation(7, 'Hello') },
    'POST /api/workspaces/1/conversations/7/messages': { status: 202, body: { submission_id: 12 } },
  })
  // The POST answers only when the test says so.
  const answered = globalThis.fetch
  let release = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  let posted = () => {}
  const done = new Promise<void>((resolve) => {
    posted = resolve
  })
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method !== 'POST') return answered(input, init)
    await held
    const res = await answered(input, init)
    posted()
    return res
  })
  render(<Page streams={fakeStreams().factory} />)
  const box = (await screen.findByLabelText('Message')) as HTMLTextAreaElement
  fireEvent.change(box, { target: { value: 'First' } })
  fireEvent.keyDown(box, { key: 'Enter' })
  fireEvent.change(box, { target: { value: 'Second, typed meanwhile' } })
  await act(async () => {
    release()
    await done
  })
  // The send has finished, and its answer has been handled.
  await act(async () => {})
  expect(box.value).toBe('Second, typed meanwhile')
})

test('an archived conversation cannot be sent to', async () => {
  window.history.replaceState(null, '', '/w/1/conversations/7')
  fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [] } },
    'GET /api/workspaces/1/conversations/7': {
      body: { ...conversation(7, 'Old'), archived_at: '2026-11-18T11:00:00.000Z' },
    },
  })
  render(<Page streams={fakeStreams().factory} />)
  expect(await screen.findByText('This conversation is archived.')).toBeTruthy()
  expect((screen.getByLabelText('Message') as HTMLTextAreaElement).disabled).toBe(true)
})

test('with no conversation, the words say what each button does', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({ 'GET /api/workspaces/1/conversations': { body: { conversations: [] } } })
  render(<Page streams={fakeStreams().factory} />)
  expect(await screen.findByText('No conversations yet.')).toBeTruthy()
  expect(screen.getByText('Start one with New, or ask the agent to build an app.')).toBeTruthy()
  expect(screen.getByRole('button', { name: 'New' })).toBeTruthy()
  expect(screen.getByRole('link', { name: 'Build an app' })).toBeTruthy()
})

// Lets the probe's promise settle under fake timers.
const settle = () => act(async () => {})

test('the stream starts again after a refusal or ops that do not fit', async () => {
  vi.useFakeTimers()
  const { sources, factory } = fakeStreams()
  const { result, unmount } = renderHook(() => useConversationStream('/events', factory, async () => false))

  // Ops before any view are ignored.
  act(() => sources[0].send({ type: 'ops', ops: [['s', ['x'], 1]] }))
  expect(result.current.view).toBeNull()

  act(() => sources[0].send({ type: 'value', value: { entries: [] } }))
  expect(result.current.view).toEqual({ entries: [] })

  // An op on a path that isn't there: the view can't be trusted, so start over.
  act(() => sources[0].send({ type: 'ops', ops: [['s', ['entries', 5, 'text'], 'x']] }))
  expect(sources[0].closed).toBe(true)
  expect(result.current.status).toBe('reconnecting')
  act(() => vi.advanceTimersByTime(RETRY_MS))
  expect(sources).toHaveLength(2)

  // A dropped connection EventSource retries by itself; a refused one (closed) is ours.
  act(() => sources[1].fail(false))
  act(() => vi.advanceTimersByTime(RETRY_MS))
  expect(sources).toHaveLength(2)
  act(() => sources[1].fail(true))
  await settle()
  act(() => vi.advanceTimersByTime(RETRY_MS))
  expect(sources).toHaveLength(3)

  unmount()
  expect(sources[2].closed).toBe(true)
})

test('refusals in a row wait longer each time, and a gone conversation stops it', async () => {
  vi.useFakeTimers()
  const { sources, factory } = fakeStreams()
  let gone = false
  const probed: string[] = []
  const probe = async (url: string) => {
    probed.push(url)
    return gone
  }
  const { result } = renderHook(() => useConversationStream('/api/workspaces/1/conversations/7/events', factory, probe))

  // 3 s, then 6 s, then 12 s, ... up to RETRY_MAX_MS.
  let wait = RETRY_MS
  for (let i = 0; i < 6; i++) {
    act(() => sources[i].fail(true))
    await settle()
    act(() => vi.advanceTimersByTime(wait - 1))
    expect(sources).toHaveLength(i + 1)
    act(() => vi.advanceTimersByTime(1))
    expect(sources).toHaveLength(i + 2)
    wait = Math.min(wait * 2, RETRY_MAX_MS)
  }
  expect(probed[0]).toBe('/api/workspaces/1/conversations/7')

  // A working stream starts the count again.
  act(() => sources[6].send({ type: 'value', value: { entries: [] } }))
  act(() => sources[6].fail(true))
  await settle()
  act(() => vi.advanceTimersByTime(RETRY_MS))
  expect(sources).toHaveLength(8)

  // The conversation is gone: no more connections.
  gone = true
  act(() => sources[7].fail(true))
  await settle()
  expect(result.current.status).toBe('gone')
  act(() => vi.advanceTimersByTime(RETRY_MAX_MS * 2))
  expect(sources).toHaveLength(8)
})

test('an expired session stops the stream and goes back to login', async () => {
  vi.useFakeTimers()
  fakeFetch({ 'GET /api/workspaces/1/conversations/7': { status: 401, body: { error: 'log in' } } })
  const lost = vi.fn()
  window.addEventListener(SESSION_LOST, lost)
  const { sources, factory } = fakeStreams()
  const { result } = renderHook(() => useConversationStream('/api/workspaces/1/conversations/7/events', factory))
  act(() => sources[0].fail(true))
  await settle()
  expect(result.current.status).toBe('gone')
  expect(lost).toHaveBeenCalled()
  act(() => vi.advanceTimersByTime(RETRY_MAX_MS * 2))
  expect(sources).toHaveLength(1)
  window.removeEventListener(SESSION_LOST, lost)
})

test('the transcript follows new text only while the reader is at the bottom', async () => {
  window.history.replaceState(null, '', '/w/1/conversations/7')
  fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [conversation(7, 'Hello')] } },
    'GET /api/workspaces/1/conversations/7': { body: conversation(7, 'Hello') },
  })
  const { sources, factory } = fakeStreams()
  render(<Page streams={factory} />)
  await waitFor(() => expect(sources).toHaveLength(1))
  const el = screen.getByLabelText('Conversation').querySelector('.ui-chat-transcript') as HTMLDivElement
  // jsdom has no layout: a transcript 1000 px tall in a 200 px window.
  Object.defineProperty(el, 'scrollHeight', { configurable: true, get: () => 1000 })
  Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => 200 })
  act(() => {
    for (const event of stream.slice(0, 5)) sources[0].send(event)
  })
  expect(el.scrollTop).toBe(1000)

  // Scrolled up to re-read: new text doesn't pull the reader down.
  el.scrollTop = 100
  fireEvent.scroll(el)
  act(() => sources[0].send(stream[5]))
  expect(el.scrollTop).toBe(100)

  // Back at the bottom: it follows again.
  el.scrollTop = 800
  fireEvent.scroll(el)
  act(() => sources[0].send(stream[6]))
  expect(el.scrollTop).toBe(1000)
})

test('ops that keep not fitting are backed off too, even after fresh views', () => {
  vi.useFakeTimers()
  const { sources, factory } = fakeStreams()
  renderHook(() => useConversationStream('/events', factory, async () => false))
  let wait = RETRY_MS
  for (let i = 0; i < 6; i++) {
    act(() => sources[i].send({ type: 'value', value: { entries: [] } }))
    act(() => sources[i].send({ type: 'ops', ops: [['s', ['entries', 5, 'text'], 'x']] }))
    act(() => vi.advanceTimersByTime(wait - 1))
    expect(sources).toHaveLength(i + 1)
    act(() => vi.advanceTimersByTime(1))
    expect(sources).toHaveLength(i + 2)
    wait = Math.min(wait * 2, RETRY_MAX_MS)
  }
})

test('a refused connection is handled once, however many errors it fires', async () => {
  vi.useFakeTimers()
  const { sources, factory } = fakeStreams()
  const probe = vi.fn(async () => false)
  renderHook(() => useConversationStream('/events', factory, probe))
  act(() => {
    sources[0].fail(true)
    sources[0].fail(true)
  })
  await settle()
  expect(probe).toHaveBeenCalledTimes(1)
  // The wait is still the first one, not doubled.
  act(() => vi.advanceTimersByTime(RETRY_MS))
  expect(sources).toHaveLength(2)
})

test('screen readers hear the agent start and reply, not each streamed word', async () => {
  window.history.replaceState(null, '', '/w/1/conversations/7')
  fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [conversation(7, 'Hello')] } },
    'GET /api/workspaces/1/conversations/7': { body: conversation(7, 'Hello') },
  })
  const { sources, factory } = fakeStreams()
  render(<Page streams={factory} />)
  await waitFor(() => expect(sources).toHaveLength(1))
  const transcript = screen.getByLabelText('Conversation').querySelector('.ui-chat-transcript') as HTMLElement
  expect(transcript.getAttribute('aria-live')).toBeNull()
  act(() => {
    for (const event of stream) sources[0].send(event)
  })
  expect(screen.getByRole('status').textContent).toBe('The agent replied.')
})

// An entry of a view or a page: one message.
const entry = (id: number, role: 'user' | 'assistant', text: string) => ({
  id,
  kind: role === 'user' ? 'pi.user' : 'pi.assistant',
  model: [{ role, content: role === 'user' ? text : [{ type: 'text', text }] }],
})
const viewOf = (entries: ReturnType<typeof entry>[], extra: Record<string, unknown> = {}) => ({
  type: 'value',
  value: { entries, docs: { 'pi.live': {} }, ...extra },
})

// Opens conversation 7, live, showing the view given.
async function openWith(routes: Parameters<typeof fakeFetch>[0], view = viewOf([])) {
  window.history.replaceState(null, '', '/w/1/conversations/7')
  const calls = fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [conversation(7, 'Hello')] } },
    'GET /api/workspaces/1/conversations/7': { body: conversation(7, 'Hello') },
    ...routes,
  })
  const { sources, factory } = fakeStreams()
  render(<Page streams={factory} />)
  await waitFor(() => expect(sources).toHaveLength(1))
  act(() => {
    sources[0].open()
    sources[0].send(view)
  })
  return { calls, es: sources[0] }
}

test("the agent's replies are Markdown, without HTML or unsafe links; the user's text is as typed", async () => {
  await openWith(
    {},
    viewOf([
      entry(1, 'user', 'Make **this** <b>bold</b>'),
      entry(
        2,
        'assistant',
        'Done: **bold**, [docs](https://example.com) <img src=x onerror="alert(1)"> [run](javascript:alert(1)) <script>alert(1)</script>',
      ),
    ]),
  )
  const [mine, reply] = screen.getAllByRole('article')
  expect(mine.querySelector('strong, b')).toBeNull()
  expect(mine.textContent).toContain('Make **this** <b>bold</b>')
  expect(reply.querySelector('strong')?.textContent).toBe('bold')
  expect(reply.querySelector('img, script, [onerror]')).toBeNull()
  expect(reply.textContent).toContain('<img src=x onerror="alert(1)">')
  const links = [...reply.querySelectorAll('a')]
  expect(links.map((a) => [a.textContent, a.getAttribute('href'), a.getAttribute('rel')])).toEqual([
    ['docs', 'https://example.com', 'noopener noreferrer'],
  ])
  expect(reply.textContent).toContain('run')
})

test('renames and archives the conversation', async () => {
  let current = conversation(7, 'Hello')
  const { calls } = await openWith({
    'GET /api/workspaces/1/conversations/7': () => ({ body: current }),
    'PATCH /api/workspaces/1/conversations/7': (call) => {
      const body = call.body as { title?: string; archived?: boolean }
      current = {
        ...current,
        ...(body.title !== undefined ? { title: body.title } : {}),
        ...(body.archived !== undefined ? { archived_at: body.archived ? '2026-11-18T11:00:00.000Z' : null } : {}),
      } as typeof current
      return { body: current }
    },
  })
  const pane = screen.getByLabelText('Conversation')
  fireEvent.click(await screen.findByRole('button', { name: 'Rename' }))
  const field = screen.getByLabelText('Title') as HTMLInputElement
  expect(field.value).toBe('Hello')
  fireEvent.change(field, { target: { value: 'Launch plan' } })
  fireEvent.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(pane.querySelector('h2')?.textContent).toBe('Launch plan'))
  expect(calls.filter((c) => c.method === 'PATCH').map((c) => c.body)).toEqual([{ title: 'Launch plan' }])

  // Escape leaves the title as it was. While the title is edited, the other actions stay, and
  // when the edit ends the focus goes back to Rename.
  fireEvent.click(screen.getByRole('button', { name: 'Rename' }))
  expect(screen.getByRole('button', { name: 'Archive' })).toBeTruthy()
  fireEvent.keyDown(screen.getByLabelText('Title'), { key: 'Escape' })
  expect(screen.queryByLabelText('Title')).toBeNull()
  await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Rename' })))
  // And so does Cancel.
  fireEvent.click(screen.getByRole('button', { name: 'Rename' }))
  screen.getByRole('button', { name: 'Archive' }).focus()
  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(screen.queryByLabelText('Title')).toBeNull()
  await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Rename' })))
  expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(1)

  fireEvent.click(screen.getByRole('button', { name: 'Archive' }))
  expect(await screen.findByText('This conversation is archived.')).toBeTruthy()
  expect((screen.getByLabelText('Message') as HTMLTextAreaElement).disabled).toBe(true)
  fireEvent.click(screen.getByRole('button', { name: 'Unarchive' }))
  await waitFor(() => expect((screen.getByLabelText('Message') as HTMLTextAreaElement).disabled).toBe(false))
  expect(calls.filter((c) => c.method === 'PATCH').map((c) => c.body)).toEqual([
    { title: 'Launch plan' },
    { archived: true },
    { archived: false },
  ])
})

test('the list shows the archived conversations on request', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [conversation(3, 'Plans')] } },
    'GET /api/workspaces/1/conversations?archived=true': { body: { conversations: [conversation(4, 'Old plans')] } },
  })
  render(<Page streams={fakeStreams().factory} />)
  expect(await screen.findByRole('link', { name: /Plans/ })).toBeTruthy()
  const toggle = screen.getByRole('button', { name: 'Show archived conversations' })
  expect(toggle.getAttribute('aria-pressed')).toBe('false')
  fireEvent.click(toggle)
  expect(await screen.findByRole('link', { name: /Old plans/ })).toBeTruthy()
  expect(toggle.getAttribute('aria-pressed')).toBe('true')
  expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('Archived')
  expect(calls.at(-1)?.path).toBe('/api/workspaces/1/conversations?archived=true')
})

test('the model is chosen among the providers the user may use', async () => {
  const { calls } = await openWith({
    'GET /api/workspaces/1/models': {
      body: {
        providers: [
          { provider: 'script', name: 'Scripted', scope: 'instance', models: [{ id: 'script-1', name: 'Scripted' }] },
          { provider: 'u2-home', name: 'My Ollama', scope: 'user', models: [{ id: 'llama', name: 'Llama' }] },
        ],
      },
    },
    'GET /api/workspaces/1/conversations/7/model': { body: { provider: 'script', model_id: 'script-1' } },
    'PUT /api/workspaces/1/conversations/7/model': (call) => ({ body: call.body }),
  })
  const select = (await screen.findByLabelText('Model')) as HTMLSelectElement
  expect([...select.options].map((o) => o.textContent)).toEqual(['Scripted', 'Llama'])
  expect(select.selectedOptions[0].textContent).toBe('Scripted')
  fireEvent.change(select, { target: { value: JSON.stringify(['u2-home', 'llama']) } })
  await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true))
  expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ provider: 'u2-home', model_id: 'llama' })
  await waitFor(() => expect(select.disabled).toBe(false))
  expect(select.selectedOptions[0].textContent).toBe('Llama')
})

test('a model the user may no longer use shows as such, and a refused choice goes back', async () => {
  await openWith({
    'GET /api/workspaces/1/models': {
      body: {
        providers: [
          { provider: 'script', name: 'Scripted', scope: 'instance', models: [{ id: 'script-1', name: 'Scripted' }] },
        ],
      },
    },
    'GET /api/workspaces/1/conversations/7/model': { body: { provider: 'gone', model_id: 'old' } },
    'PUT /api/workspaces/1/conversations/7/model': { status: 400, body: { error: 'unknown model' } },
  })
  const select = (await screen.findByLabelText('Model')) as HTMLSelectElement
  expect(select.selectedOptions[0].textContent).toBe('old (gone, not available)')
  fireEvent.change(select, { target: { value: JSON.stringify(['script', 'script-1']) } })
  expect(await screen.findByText('unknown model')).toBeTruthy()
  expect(select.selectedOptions[0].textContent).toBe('old (gone, not available)')
})

test('while the agent works, a message steers it and Stop stops it', async () => {
  const { calls, es } = await openWith(
    {
      'POST /api/workspaces/1/conversations/7/messages': { status: 202, body: { submission_id: 12 } },
      'POST /api/workspaces/1/conversations/7/stop': { status: 204 },
    },
    viewOf([entry(1, 'user', 'Write a long essay')], { docs: { 'pi.live': { run: { taskId: 3 } } } }),
  )
  const box = screen.getByLabelText('Message')
  fireEvent.change(box, { target: { value: 'Shorter, please' } })
  fireEvent.keyDown(box, { key: 'Enter' })
  await waitFor(() => expect(calls.some((c) => c.path.endsWith('/messages'))).toBe(true))
  expect(calls.find((c) => c.path.endsWith('/messages'))?.body).toMatchObject({
    content: 'Shorter, please',
    when_busy: 'steer',
  })

  fireEvent.click(screen.getByRole('button', { name: 'Stop' }))
  await waitFor(() => expect(calls.some((c) => c.method === 'POST' && c.path.endsWith('/stop'))).toBe(true))

  // Once the work has ended, there is nothing to stop, and a message starts new work.
  act(() => es.send(viewOf([entry(1, 'user', 'Write a long essay')])))
  expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull()
  fireEvent.change(box, { target: { value: 'Next' } })
  fireEvent.click(screen.getByRole('button', { name: 'Send' }))
  await waitFor(() => expect(calls.filter((c) => c.path.endsWith('/messages'))).toHaveLength(2))
  expect(calls.filter((c) => c.path.endsWith('/messages'))[1].body).not.toHaveProperty('when_busy')
})

test('earlier messages go in above what the reader is looking at, which stays where it is', async () => {
  await openWith(
    {
      'GET /api/workspaces/1/conversations/7/entries?before=8': {
        body: { entries: [entry(5, 'user', 'Older question'), entry(6, 'assistant', 'Older answer')] },
      },
    },
    viewOf([entry(8, 'user', 'Newer question'), entry(9, 'assistant', 'Newer answer')], { before: 7 }),
  )
  const el = screen.getByLabelText('Conversation').querySelector('.ui-chat-transcript') as HTMLDivElement
  // jsdom has no layout: 1000 px of messages, the reader 150 px from the top of them.
  let height = 1000
  Object.defineProperty(el, 'scrollHeight', { configurable: true, get: () => height })
  Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => 200 })
  el.scrollTop = 150
  fireEvent.scroll(el)
  fireEvent.click(screen.getByRole('button', { name: 'Load earlier messages' }))
  // The earlier messages add 600 px above.
  height = 1600
  await screen.findByText('Older question')
  // What the reader was looking at is 600 px further down, and so is the view.
  expect(el.scrollTop).toBe(750)
})

test('earlier messages load from before the stream, and stay as the stream moves on', async () => {
  const { calls, es } = await openWith(
    {
      'GET /api/workspaces/1/conversations/7/entries?before=8': {
        body: { entries: [entry(5, 'user', 'Older question'), entry(6, 'assistant', 'Older answer')], before: 3 },
      },
      'GET /api/workspaces/1/conversations/7/entries?before=5': {
        body: { entries: [entry(2, 'user', 'The first question')] },
      },
    },
    viewOf([entry(8, 'user', 'Newer question'), entry(9, 'assistant', 'Newer answer')], { before: 7 }),
  )
  const texts = () => screen.getAllByRole('article').map((a) => a.textContent?.replace(/^(You|Agent):/, ''))
  expect(texts()).toEqual(['Newer question', 'Newer answer'])

  fireEvent.click(screen.getByRole('button', { name: 'Load earlier messages' }))
  await waitFor(() => expect(texts()).toEqual(['Older question', 'Older answer', 'Newer question', 'Newer answer']))

  // The window drops its oldest entry: what was loaded, and that entry, stay.
  act(() => es.send(viewOf([entry(9, 'assistant', 'Newer answer'), entry(10, 'user', 'Newest')], { before: 8 })))
  expect(texts()).toEqual(['Older question', 'Older answer', 'Newer question', 'Newer answer', 'Newest'])

  fireEvent.click(screen.getByRole('button', { name: 'Load earlier messages' }))
  await waitFor(() => expect(texts()[0]).toBe('The first question'))
  // The conversation's start is shown: nothing more to load.
  expect(screen.queryByRole('button', { name: 'Load earlier messages' })).toBeNull()
  expect(calls.filter((c) => c.path.includes('/entries')).map((c) => c.path)).toEqual([
    '/api/workspaces/1/conversations/7/entries?before=8',
    '/api/workspaces/1/conversations/7/entries?before=5',
  ])
})

test('a page that fails to load says so, and can be tried again', async () => {
  await openWith(
    {
      'GET /api/workspaces/1/conversations/7/entries?before=8': {
        status: 503,
        body: { error: 'the agent runtime is off' },
      },
    },
    viewOf([entry(8, 'user', 'Newer question')], { before: 7 }),
  )
  fireEvent.click(screen.getByRole('button', { name: 'Load earlier messages' }))
  // Said at once, as an alert.
  expect((await screen.findByRole('alert')).textContent).toContain('the agent runtime is off')
  expect((screen.getByRole('button', { name: 'Load earlier messages' }) as HTMLButtonElement).disabled).toBe(false)
})

// Apps the agent saved, shown in the conversation.

const everything = {
  roles: ['builder', 'publisher', 'user', 'data_manager', 'reader'],
  restricted: false,
  view: true,
  design: true,
  publish: true,
  submit: true,
  manage_roles: true,
  read_records: 'all',
  modify_records: 'all',
}
const savedApp = (over: Record<string, unknown> = {}) => ({
  id: 1,
  space_id: 2,
  name: 'Expenses',
  archived_at: null,
  version: 1,
  rows: 1,
  access: everything,
  live: {
    number: 1,
    definition_version: 1,
    requires: ['component.Text'],
    published_at: '2026-11-18T10:00:00.000Z',
    published_by: null,
    live: true,
  },
  unpublished_changes: false,
  draft: { definition_version: 1, requires: ['component.Text'] },
  ...over,
})

// An entry where the agent called tool name, and the entry of its result: text, or a refusal.
function toolEntries(id: number, name: string, result: { text: string; isError?: boolean }, args: unknown = {}) {
  return [
    {
      id,
      kind: 'pi.assistant',
      model: [{ role: 'assistant', content: [{ type: 'toolCall', id: `c${id}`, name, arguments: args }] }],
    },
    {
      id: id + 1,
      kind: 'pi.tool',
      model: [
        {
          role: 'toolResult',
          toolCallId: `c${id}`,
          toolName: name,
          content: [{ type: 'text', text: result.text }],
          isError: result.isError ?? false,
        },
      ],
    },
  ]
}

// Opens conversation 7 and shows it with these entries.
async function showEntries(entries: unknown[], routes: Record<string, Parameters<typeof fakeFetch>[0][string]>) {
  window.history.replaceState(null, '', '/w/1/conversations/7')
  const calls = fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [conversation(7, 'Apps')] } },
    'GET /api/workspaces/1/conversations/7': { body: conversation(7, 'Apps') },
    ...routes,
  })
  const { sources, factory } = fakeStreams()
  render(<Page streams={factory} />)
  await waitFor(() => expect(sources).toHaveLength(1))
  act(() => {
    sources[0].open()
    sources[0].send({ type: 'value', value: { entries } })
  })
  return calls
}

const appCalls = (calls: Call[]) => calls.filter((c) => c.path.includes('/apps'))

test("a saved app's screen shows in the conversation, as the server has it", async () => {
  // What the model sent: never drawn.
  const args = {
    space_id: 2,
    name: 'Expenses',
    definition: { components: [{ id: 'root', component: 'Text', text: 'From the arguments' }] },
  }
  const calls = await showEntries(toolEntries(1, 'create_app', { text: JSON.stringify(savedApp()) }, args), {
    'GET /api/workspaces/1/spaces/2/apps/1': { body: savedApp() },
    'GET /api/workspaces/1/spaces/2/apps/1/surface?release=1': { body: surface },
  })

  // The preview is a lazily loaded component that fetches the app and then its screen: wait for
  // each thing it shows (findBy… waits as long as src/test/setup.ts says), never for a time.
  // The app's frame is drawn at once with its loading shape, and drawn anew once the renderer is in.
  await screen.findByText('Expense tracker')
  const app = screen.getByRole('region', { name: 'Expenses' })
  await within(app).findByText('Expense tracker')
  await within(app).findByText('Coffee')
  // What it shows is labelled: the live release, which everyone uses.
  await screen.findByText('Live · release 1')
  expect(screen.getByText(wholeText('Used create_app'))).toBeTruthy()
  expect(screen.queryByText('From the arguments')).toBeNull()
  expect(appCalls(calls).map((c) => c.path)).toEqual([
    '/api/workspaces/1/spaces/2/apps/1',
    '/api/workspaces/1/spaces/2/apps/1/surface?release=1',
  ])
})

test("a live embed stays labelled Live and its buttons still act on the app's real rows", async () => {
  const calls = await showEntries(toolEntries(1, 'update_app', { text: JSON.stringify(savedApp({ version: 2 })) }), {
    'GET /api/workspaces/1/spaces/2/apps/1': { body: savedApp({ version: 2 }) },
    'GET /api/workspaces/1/spaces/2/apps/1/surface?release=1': { body: surface },
    'POST /api/workspaces/1/spaces/2/apps/1/actions': { body: afterAdd },
  })
  await screen.findByText('Live · release 1')
  await screen.findByText('Coffee')
  fireEvent.click(await screen.findByRole('button', { name: 'Delete' }))
  await waitFor(() => expect(appCalls(calls).some((c) => c.method === 'POST')).toBe(true))
  const posted = appCalls(calls).filter((c) => c.method === 'POST')
  // The live app's own actions, naming the release the screen was drawn from; never a preview.
  expect(posted.map((c) => c.path)).toEqual(['/api/workspaces/1/spaces/2/apps/1/actions'])
  expect(posted[0].body).toMatchObject({ name: 'delete_expense', release: 1 })
})

test('the open link opens the app on the Apps page', async () => {
  await showEntries(toolEntries(1, 'update_app', { text: JSON.stringify(savedApp({ version: 2 })) }), {
    'GET /api/workspaces/1/spaces/2/apps/1': { body: savedApp({ version: 2 }) },
    'GET /api/workspaces/1/spaces/2/apps/1/surface?release=1': { body: surface },
  })
  const open = await screen.findByRole('link', { name: 'Open Expenses in Apps' })
  expect(open.getAttribute('href')).toBe('/w/1/apps/2-1')
  fireEvent.click(open)
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/apps/2-1'))
})

test('the agent changed the app: the conversation shows the live release, and the draft only on request', async () => {
  const calls = await showEntries(toolEntries(1, 'update_app', { text: JSON.stringify(savedApp({ version: 2 })) }), {
    'GET /api/workspaces/1/spaces/2/apps/1': { body: savedApp({ version: 2, unpublished_changes: true }) },
    'GET /api/workspaces/1/spaces/2/apps/1/surface?release=1': { body: surface },
    'POST /api/workspaces/1/spaces/2/apps/1/preview': { body: { messages: surface, rows: [] } },
  })
  // The live release is what people use, and it is said so; the agent's changes aren't in it.
  await screen.findByText('Live · release 1')
  await screen.findByText(/It has changes that aren’t published yet/)
  expect(appCalls(calls).some((c) => c.path.endsWith('/preview'))).toBe(false)
  fireEvent.click(await screen.findByRole('button', { name: 'Preview the draft' }))
  await screen.findByText('Preview · sample data')
  await waitFor(() => expect(appCalls(calls).some((c) => c.path.endsWith('/preview'))).toBe(true))
})

test('a new app the agent made is a draft preview for its builders and a note for the rest', async () => {
  const fresh = { live: null, unpublished_changes: true }
  await showEntries(toolEntries(1, 'create_app', { text: JSON.stringify(savedApp(fresh)) }), {
    'GET /api/workspaces/1/spaces/2/apps/1': { body: savedApp(fresh) },
    'POST /api/workspaces/1/spaces/2/apps/1/preview': { body: { messages: surface, rows: [] } },
  })
  await screen.findByText('Preview · sample data')
  await screen.findByText('Coffee')
  cleanup()

  const user = { ...everything, design: false, publish: false, manage_roles: false }
  await showEntries(toolEntries(1, 'create_app', { text: JSON.stringify(savedApp(fresh)) }), {
    'GET /api/workspaces/1/spaces/2/apps/1': { body: savedApp({ ...fresh, access: user, draft: undefined }) },
  })
  await screen.findByText(/This app isn’t published yet/)
})

test('a refused or failed app tool call shows no app', async () => {
  const calls = await showEntries(
    [
      ...toolEntries(1, 'create_app', { text: 'error: the definition has no root component', isError: true }),
      ...toolEntries(3, 'update_app', {
        text: 'the server is unreachable right now: try again shortly',
        isError: true,
      }),
      // Successful in shape, but flagged as an error: still nothing.
      ...toolEntries(5, 'create_app', { text: JSON.stringify(savedApp()), isError: true }),
    ],
    {},
  )
  await screen.findByText(wholeText('Used update_app'))
  expect(screen.getAllByText(wholeText('Used create_app'))).toHaveLength(2)
  expect(screen.queryByRole('link', { name: /Open/ })).toBeNull()
  expect(screen.queryByText('Loading…')).toBeNull()
  expect(appCalls(calls)).toEqual([])
})

test('an app the user cannot read, or that was taken offline, is a note', async () => {
  const calls = await showEntries(
    [
      ...toolEntries(1, 'create_app', { text: JSON.stringify(savedApp({ id: 1, space_id: 2 })) }),
      ...toolEntries(3, 'create_app', { text: JSON.stringify(savedApp({ id: 5, space_id: 2, name: 'Old' })) }),
    ],
    {
      // Gone, moved, or in a space the user was removed from: the server says 404 for each.
      'GET /api/workspaces/1/spaces/2/apps/1': { status: 404, body: { error: 'not found' } },
      'GET /api/workspaces/1/spaces/2/apps/5': {
        body: savedApp({ id: 5, name: 'Old', archived_at: '2026-11-18T12:00:00.000Z' }),
      },
    },
  )
  expect(await screen.findByText(/This app isn’t available/)).toBeTruthy()
  expect(await screen.findByText('Old is offline: it can’t be launched.')).toBeTruthy()
  // Nothing is launched; the offline app can still be opened, to be restored there.
  expect(await screen.findByRole('link', { name: 'Open Old in Apps' })).toBeTruthy()
  expect(appCalls(calls).filter((c) => c.path.includes('/surface'))).toEqual([])
  expect(screen.queryByRole('link', { name: 'Open the app in Apps' })).toBeNull()
})

test('each app shows once, at its latest save, and older apps only link', async () => {
  const entries = [1, 2, 3, 4].flatMap((id) =>
    toolEntries(id * 10, 'create_app', { text: JSON.stringify(savedApp({ id, name: `App ${id}` })) }),
  )
  // App 4 again, changed: shown here, not at its first save.
  entries.push(
    ...toolEntries(50, 'update_app', { text: JSON.stringify(savedApp({ id: 4, name: 'App 4', version: 2 })) }),
  )
  const routes: Record<string, { body: unknown }> = {}
  for (const id of [1, 2, 3, 4]) {
    routes[`GET /api/workspaces/1/spaces/2/apps/${id}`] = { body: savedApp({ id, name: `App ${id}` }) }
    routes[`GET /api/workspaces/1/spaces/2/apps/${id}/surface?release=1`] = { body: [] }
  }
  const calls = await showEntries(entries, routes)

  await screen.findByRole('link', { name: 'Open App 2 in Apps' })
  await screen.findByRole('link', { name: 'Open App 3 in Apps' })
  await screen.findByRole('link', { name: 'Open App 4 in Apps' })
  // Only the latest three apps load, each once; the first and app 4's first save link.
  expect(screen.getAllByRole('link', { name: 'Open the app' }).map((a) => a.getAttribute('href'))).toEqual([
    '/w/1/apps/2-1',
    '/w/1/apps/2-4',
  ])
  expect(
    appCalls(calls)
      .filter((c) => !c.path.includes('/surface'))
      .map((c) => c.path)
      .sort(),
  ).toEqual([
    '/api/workspaces/1/spaces/2/apps/2',
    '/api/workspaces/1/spaces/2/apps/3',
    '/api/workspaces/1/spaces/2/apps/4',
  ])
})
