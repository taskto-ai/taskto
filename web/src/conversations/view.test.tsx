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

test("a conversation that can't be read can't be sent to, and the composer says why", async () => {
  fakeFetch({ 'GET /api/workspaces/1/conversations/7': { status: 404, body: { error: 'not found' } } })
  render(view(7, fakeStreams().factory))
  const box = (await screen.findByLabelText('Message')) as HTMLTextAreaElement
  await waitFor(() => expect(box.disabled).toBe(true))
  const why = document.getElementById(box.getAttribute('aria-describedby') ?? '')
  expect(why?.textContent).toBe("This conversation can't be read, so nothing can be sent to it.")
  expect((screen.getByRole('button', { name: 'Send' }) as HTMLButtonElement).disabled).toBe(true)
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
