// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { fakeFetch, fakeStreams } from '../test/fakes'
import { ConversationView } from './ConversationView'
import turn from './testdata/turn-changes.json'

const conversation = (id: number) => ({
  id,
  workspace_id: 1,
  title: `Talk ${id}`,
  archived_at: null,
  created_at: '2026-11-18T10:00:00.000Z',
  updated_at: '2026-11-18T10:00:00.000Z',
})

beforeEach(() => window.history.replaceState(null, '', '/w/1/conversations/7'))
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

function view(id: number, factory: ReturnType<typeof fakeStreams>['factory'], buildHref?: string) {
  return (
    <ConversationView
      key={id}
      workspace={1}
      id={id}
      backHref="/w/1/conversations"
      onChanged={() => {}}
      streams={factory}
      buildHref={buildHref}
    />
  )
}

test("the stream's state is said in words in the head", async () => {
  fakeFetch({ 'GET /api/workspaces/1/conversations/7': { body: conversation(7) } })
  const { sources, factory } = fakeStreams()
  render(view(7, factory))
  const head = await screen.findByRole('heading', { name: 'Talk 7' })
  const pane = screen.getByRole('region', { name: 'Conversation' })
  expect(within(pane).getByText('Connecting…')).toBeTruthy()
  await waitFor(() => expect(sources).toHaveLength(1))
  act(() => sources[0].open())
  expect(within(pane).getByText('Live')).toBeTruthy()
  expect(head).toBeTruthy()
})

test("the menu holds the builder's link, which opens the builder", async () => {
  fakeFetch({ 'GET /api/workspaces/1/conversations/7': { body: conversation(7) } })
  render(view(7, fakeStreams().factory, '/w/1/apps/build/conversation/7'))
  await screen.findByRole('heading', { name: 'Talk 7' })
  fireEvent.click(screen.getByRole('button', { name: 'More actions' }))
  fireEvent.click(await screen.findByRole('menuitem', { name: 'Build an app in this conversation' }))
  expect(window.location.pathname).toBe('/w/1/apps/build/conversation/7')
})

const MODELS = {
  'GET /api/workspaces/1/models': {
    body: {
      providers: [{ provider: 'script', name: 'Scripted', scope: 'instance', models: [{ id: 's', name: 'S' }] }],
    },
  },
  'GET /api/workspaces/1/conversations/7/model': { body: { provider: 'script', model_id: 's' } },
}

const box = () => screen.getByLabelText('Message') as HTMLTextAreaElement

for (const status of [403, 404]) {
  test(`a conversation the server refuses (${status}) can't be sent to, and the composer says why`, async () => {
    fakeFetch({ ...MODELS, 'GET /api/workspaces/1/conversations/7': { status, body: { error: 'no' } } })
    render(view(7, fakeStreams().factory))
    await screen.findByLabelText('Message')
    await waitFor(() => expect(box().disabled).toBe(true))
    const why = document.getElementById(box().getAttribute('aria-describedby') ?? '')
    expect(why?.textContent).toBe("This conversation can't be read, so nothing can be sent to it.")
    expect((screen.getByRole('button', { name: 'Send' }) as HTMLButtonElement).disabled).toBe(true)
    expect(((await screen.findByLabelText('Model')) as HTMLSelectElement).disabled).toBe(true)
    expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull()
  })
}

test('a first read that fails for another reason leaves the composer usable, and is tried again', async () => {
  let fail = true
  fakeFetch({
    ...MODELS,
    'GET /api/workspaces/1/conversations/7': () =>
      fail ? { status: 502, body: { error: 'bad gateway' } } : { body: conversation(7) },
  })
  const { sources, factory } = fakeStreams()
  render(view(7, factory))
  const retry = await screen.findByRole('button', { name: 'Try again' })
  expect(box().disabled).toBe(false)
  expect(box().getAttribute('aria-describedby')).toBeNull()
  expect(((await screen.findByLabelText('Model')) as HTMLSelectElement).disabled).toBe(false)

  fail = false
  await waitFor(() => expect(sources).toHaveLength(1))
  act(() => sources[0].open())
  expect(await screen.findByRole('heading', { name: 'Talk 7' })).toBeTruthy()
  await waitFor(() => expect(retry.isConnected).toBe(false))
})

test('Try again reads the conversation again', async () => {
  let fail = true
  fakeFetch({
    'GET /api/workspaces/1/conversations/7': () =>
      fail ? { status: 503, body: { error: 'unavailable' } } : { body: conversation(7) },
  })
  render(view(7, fakeStreams().factory))
  const retry = await screen.findByRole('button', { name: 'Try again' })
  fail = false
  fireEvent.click(retry)
  expect(await screen.findByRole('heading', { name: 'Talk 7' })).toBeTruthy()
})

test('a failed read again keeps the conversation shown and the composer usable', async () => {
  let fail = false
  fakeFetch({
    'GET /api/workspaces/1/conversations/7': () =>
      fail ? { status: 502, body: { error: 'bad gateway' } } : { body: conversation(7) },
    'PATCH /api/workspaces/1/conversations/7': () => {
      fail = true
      return { body: conversation(7) }
    },
  })
  render(view(7, fakeStreams().factory))
  await screen.findByRole('heading', { name: 'Talk 7' })
  fireEvent.click(screen.getByRole('button', { name: 'Rename' }))
  fireEvent.click(screen.getByRole('button', { name: 'Save' }))
  expect(await screen.findByRole('button', { name: 'Try again' })).toBeTruthy()
  expect(screen.getByRole('heading', { name: 'Talk 7' })).toBeTruthy()
  expect(box().disabled).toBe(false)
  expect(box().getAttribute('aria-describedby')).toBeNull()
})

test('two views on one page each have their own fields', async () => {
  fakeFetch({
    'GET /api/workspaces/1/conversations/7': { body: conversation(7) },
    'GET /api/workspaces/1/conversations/8': { body: conversation(8) },
  })
  const { factory } = fakeStreams()
  render(
    <>
      {view(7, factory)}
      {view(8, factory)}
    </>,
  )
  await screen.findByRole('heading', { name: 'Talk 8' })
  const boxes = screen.getAllByLabelText('Message')
  expect(boxes).toHaveLength(2)
  expect(boxes[0].id).not.toBe(boxes[1].id)
  expect(document.getElementById('tt-message')).toBeNull()
})

test("a refusal that answers a read since superseded doesn't turn the composer off", async () => {
  let refuse7: (a: { status: number; body: unknown }) => void = () => {}
  fakeFetch({
    'GET /api/workspaces/1/conversations/7': () =>
      new Promise((done) => {
        refuse7 = done
      }),
    'GET /api/workspaces/1/conversations/8': { status: 502, body: { error: 'bad gateway' } },
  })
  const { factory } = fakeStreams()
  const props = { workspace: 1, backHref: '/w/1/conversations', onChanged: () => {}, streams: factory }
  const { rerender } = render(<ConversationView {...props} id={7} />)
  await waitFor(() => expect(refuse7).not.toBe(undefined))
  rerender(<ConversationView {...props} id={8} />)
  await screen.findByRole('button', { name: 'Try again' })
  await act(async () => refuse7({ status: 404, body: { error: 'not found' } }))
  expect(box().disabled).toBe(false)
  expect(box().getAttribute('aria-describedby')).toBeNull()
})

async function showTurn(entries: unknown[]) {
  fakeFetch({ 'GET /api/workspaces/1/conversations/7': { body: conversation(7) } })
  const { sources, factory } = fakeStreams()
  render(view(7, factory))
  await waitFor(() => expect(sources).toHaveLength(1))
  act(() => {
    sources[0].open()
    sources[0].send({ type: 'value', value: { entries } })
  })
  return sources[0]
}

test('the answer ends with a list of what the turn changed, each opening its page', async () => {
  await showTurn(turn)
  const title = await screen.findByText('Changed in this turn (3)')
  const box = title.closest('details') as HTMLDetailsElement
  expect(box.open).toBe(true)
  const links = within(box).getAllByRole('link')
  expect(links.map((l) => [l.textContent, l.getAttribute('href')])).toEqual([
    ['a.md', '/w/1/knowledge/3%3Anotes%2Fa.md'],
    ['Call the bank', '/w/1/boards/3%3A4%3A9'],
    ['Record 12', '/w/1/apps/2-5'],
  ])
  expect(within(box).getByText('notes')).toBeTruthy()
  expect(within(box).getAllByText('Created')).toHaveLength(3)
  const answer = screen.getByText('Done: the notes, the card and the record are in.').closest('article') as HTMLElement
  expect(answer.contains(box)).toBe(true)
  expect(screen.getAllByText('Changed in this turn (3)')).toHaveLength(1)
})

test('a click on a listed item goes to its page without a reload', async () => {
  await showTurn(turn)
  fireEvent.click(await screen.findByRole('link', { name: 'Call the bank' }))
  expect(window.location.pathname).toBe('/w/1/boards/3%3A4%3A9')
})

test('what was deleted or archived is listed without a link', async () => {
  const call = (id: string, name: string, args: unknown) => ({ type: 'toolCall', id, name, arguments: args })
  const result = (id: string, name: string, text: unknown) => ({
    role: 'toolResult',
    toolCallId: id,
    toolName: name,
    content: [{ type: 'text', text: JSON.stringify(text) }],
    isError: false,
  })
  await showTurn([
    { id: 1, kind: 'pi.user', model: [{ role: 'user', content: 'Clean up' }] },
    {
      id: 2,
      kind: 'pi.assistant',
      model: [
        {
          role: 'assistant',
          content: [
            call('a', 'delete_doc', { space_id: 3, path: 'old/a.md', base_version: 'v1' }),
            call('b', 'archive_card', { space_id: 3, board_id: 4, card_id: 9, version: 1 }),
          ],
        },
      ],
    },
    { id: 3, kind: 'pi.tool', model: [result('a', 'delete_doc', { commit: 'c' })] },
    { id: 4, kind: 'pi.tool', model: [result('b', 'archive_card', { id: 9, board_id: 4, title: 'Old card' })] },
    { id: 5, kind: 'pi.assistant', model: [{ role: 'assistant', content: [{ type: 'text', text: 'Cleaned.' }] }] },
  ])
  const box = (await screen.findByText('Changed in this turn (2)')).closest('details') as HTMLElement
  expect(within(box).queryAllByRole('link')).toHaveLength(0)
  expect(within(box).getByText('a.md')).toBeTruthy()
  expect(within(box).getByText('Old card')).toBeTruthy()
  expect(within(box).getByText('Deleted')).toBeTruthy()
  expect(within(box).getByText('Archived')).toBeTruthy()
})

test('failed and read-only calls list nothing', async () => {
  await showTurn(turn.slice(0, 1).concat(turn.slice(1, 3), turn.slice(5, 6)))
  expect(screen.queryByText(/Changed in this turn/)).toBeNull()
})

test("a running turn's list grows as its tools finish", async () => {
  const source = await showTurn(turn.slice(0, 4))
  expect(await screen.findByText('Changed in this turn (1)')).toBeTruthy()
  act(() => source.send({ type: 'value', value: { entries: turn.slice(0, 5) } }))
  expect(await screen.findByText('Changed in this turn (2)')).toBeTruthy()
  act(() => source.send({ type: 'value', value: { entries: turn } }))
  expect(await screen.findByText('Changed in this turn (3)')).toBeTruthy()
  expect(screen.getAllByText(/Changed in this turn/)).toHaveLength(1)
})

test("a turn's list doesn't carry into the next turn's answer", async () => {
  await showTurn([
    ...turn,
    { id: 20, kind: 'pi.user', model: [{ role: 'user', content: 'Thanks' }] },
    { id: 21, kind: 'pi.assistant', model: [{ role: 'assistant', content: [{ type: 'text', text: 'Welcome.' }] }] },
  ])
  await screen.findByText('Welcome.')
  expect(screen.getAllByText(/Changed in this turn/)).toHaveLength(1)
  const first = screen.getByText('Done: the notes, the card and the record are in.').closest('article') as HTMLElement
  expect(within(first).getByText('Changed in this turn (3)')).toBeTruthy()
})

test('the transcript follows a list that appears under the newest answer while the reader is at the bottom', async () => {
  const source = await showTurn(turn.slice(0, 3))
  const el = screen.getByRole('region', { name: 'Conversation' }).querySelector('.ui-chat-transcript') as HTMLDivElement
  Object.defineProperty(el, 'scrollHeight', { configurable: true, get: () => 1000 })
  Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => 200 })
  el.scrollTop = 800
  fireEvent.scroll(el)
  act(() => source.send({ type: 'value', value: { entries: turn.slice(0, 4) } }))
  await screen.findByText('Changed in this turn (1)')
  expect(el.scrollTop).toBe(1000)

  el.scrollTop = 100
  fireEvent.scroll(el)
  act(() => source.send({ type: 'value', value: { entries: turn.slice(0, 5) } }))
  await screen.findByText('Changed in this turn (2)')
  expect(el.scrollTop).toBe(100)
})
