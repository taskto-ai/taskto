// @vitest-environment jsdom

// What the conversation's view adds to the parts: the stream's state in words, the menu of the
// less used actions, a composer that says why it is off, and ids of its own when the view is on
// a page twice.

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { fakeFetch, fakeStreams } from '../test/fakes'
import { ConversationView } from './ConversationView'

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
    // Nor can its model be changed; and there is nothing to try again.
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

  // The server is back: the stream connects, and the conversation is read again by itself.
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
  // A rename, then its read again fails.
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
    // Conversation 7's answer comes late, after the view moved on to 8.
    'GET /api/workspaces/1/conversations/7': () =>
      new Promise((done) => {
        refuse7 = done
      }),
    // And 8's read fails for a reason that is no refusal: its composer stays usable.
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
