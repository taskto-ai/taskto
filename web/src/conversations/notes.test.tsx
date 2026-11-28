// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { fakeFetch, fakeStreams } from '../test/fakes'
import { claimDrafts, forgetDrafts } from '../uiState'
import { ConversationView } from './ConversationView'

const conversation = {
  id: 7,
  workspace_id: 1,
  title: 'Talk 7',
  archived_at: null,
  created_at: '2026-11-18T10:00:00.000Z',
  updated_at: '2026-11-18T10:00:00.000Z',
}

beforeEach(() => window.history.replaceState(null, '', '/w/1/conversations/7'))
afterEach(() => {
  cleanup()
  forgetDrafts()
  vi.unstubAllGlobals()
})

function view(factory: ReturnType<typeof fakeStreams>['factory']) {
  return <ConversationView workspace={1} id={7} backHref="/w/1/conversations" onChanged={() => {}} streams={factory} />
}

const box = () => screen.getByLabelText('Message') as HTMLTextAreaElement

test('the toggle posts a note to /notes and a message to /messages', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/conversations/7': { body: conversation },
    'POST /api/workspaces/1/conversations/7/notes': { status: 202, body: { submission_id: 5 } },
    'POST /api/workspaces/1/conversations/7/messages': { status: 202, body: { submission_id: 6 } },
  })
  render(view(fakeStreams().factory))
  await screen.findByRole('heading', { name: 'Talk 7' })
  expect(box().placeholder).toBe('Message the agent')

  fireEvent.click(screen.getByRole('radio', { name: 'Note' }))
  expect(box().placeholder).toBe('Add a note for the record')
  expect(screen.getByText('A note stays in this conversation but the agent never sees it.')).toBeTruthy()
  fireEvent.change(box(), { target: { value: 'checked with finance' } })
  fireEvent.click(screen.getByRole('button', { name: 'Add note' }))
  await waitFor(() => expect(box().value).toBe(''))

  const posts = calls.filter((c) => c.method === 'POST')
  expect(posts).toHaveLength(1)
  expect(posts[0].path).toBe('/api/workspaces/1/conversations/7/notes')
  expect(posts[0].body).toMatchObject({ content: 'checked with finance' })
  expect(Object.keys(posts[0].body as object).sort()).toEqual(['content', 'request_id'])

  fireEvent.click(screen.getByRole('radio', { name: 'To the agent' }))
  fireEvent.change(box(), { target: { value: 'now the agent' } })
  fireEvent.click(screen.getByRole('button', { name: 'Send' }))
  await waitFor(() => expect(calls.filter((c) => c.method === 'POST')).toHaveLength(2))
  expect(calls.filter((c) => c.method === 'POST')[1].path).toBe('/api/workspaces/1/conversations/7/messages')
})

test('Enter in note mode adds a note, even while the agent runs', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/conversations/7': { body: conversation },
    'POST /api/workspaces/1/conversations/7/notes': { status: 202, body: { submission_id: 5 } },
  })
  const { sources, factory } = fakeStreams()
  render(view(factory))
  await screen.findByRole('heading', { name: 'Talk 7' })
  await waitFor(() => expect(sources).toHaveLength(1))
  act(() => {
    sources[0].open()
    sources[0].send({ type: 'value', value: { entries: [], docs: { 'pi.live': { run: { id: 1 } } } } })
  })
  await screen.findByRole('button', { name: 'Stop' })

  fireEvent.click(screen.getByRole('radio', { name: 'Note' }))
  expect(screen.queryByRole('button', { name: 'Queue' })).toBeNull()
  expect(screen.getByRole('button', { name: 'Stop' })).toBeTruthy()
  fireEvent.change(box(), { target: { value: 'while it works' } })
  fireEvent.keyDown(box(), { key: 'Enter' })
  await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
  const post = calls.find((c) => c.method === 'POST')
  expect(post?.path).toBe('/api/workspaces/1/conversations/7/notes')
  expect(post?.body).not.toHaveProperty('when_busy')
})

test('a note in the stream shows with its label, in order, and as Markdown', async () => {
  fakeFetch({ 'GET /api/workspaces/1/conversations/7': { body: conversation } })
  const { sources, factory } = fakeStreams()
  render(view(factory))
  await screen.findByRole('heading', { name: 'Talk 7' })
  await waitFor(() => expect(sources).toHaveLength(1))
  act(() => {
    sources[0].open()
    sources[0].send({
      type: 'value',
      value: {
        entries: [
          { id: 1, kind: 'pi.user', model: [{ role: 'user', content: 'What are the numbers?' }] },
          { id: 2, kind: 'taskto.note', data: { v: 1, author: 1, text: 'checked with **finance**' } },
        ],
      },
    })
  })
  const note = await screen.findByText('Note · the agent doesn’t see this')
  const article = note.closest('article') as HTMLElement
  expect(article.getAttribute('data-role')).toBe('note')
  expect(article.querySelector('strong')?.textContent).toBe('finance')
  const articles = [...document.querySelectorAll('article')].map((a) => a.getAttribute('data-role'))
  expect(articles).toEqual(['user', 'note'])
})

test('a refused note shows the translated reason and keeps the text', async () => {
  fakeFetch({
    'GET /api/workspaces/1/conversations/7': { body: conversation },
    'POST /api/workspaces/1/conversations/7/notes': {
      status: 400,
      body: { error: 'x', code: 'conversation_note_invalid', params: { max: 8000 } },
    },
  })
  render(view(fakeStreams().factory))
  await screen.findByRole('heading', { name: 'Talk 7' })
  fireEvent.click(screen.getByRole('radio', { name: 'Note' }))
  fireEvent.change(box(), { target: { value: 'too much' } })
  fireEvent.click(screen.getByRole('button', { name: 'Add note' }))
  expect(await screen.findByText('A note must have 1 to 8,000 characters.')).toBeTruthy()
  expect(box().value).toBe('too much')
})

test('an unsent note comes back as a note after a reload, and is still posted to /notes', async () => {
  claimDrafts(1)
  const calls = fakeFetch({
    'GET /api/workspaces/1/conversations/7': { body: conversation },
    'POST /api/workspaces/1/conversations/7/notes': { status: 202, body: { submission_id: 5 } },
    'POST /api/workspaces/1/conversations/7/messages': { status: 202, body: { submission_id: 6 } },
  })
  const first = render(view(fakeStreams().factory))
  await screen.findByRole('heading', { name: 'Talk 7' })
  fireEvent.click(screen.getByRole('radio', { name: 'Note' }))
  fireEvent.change(box(), { target: { value: 'for the record' } })
  await waitFor(() =>
    expect(localStorage.getItem('taskto:note:1:/api/workspaces/1/conversations/7')).toBe('for the record'),
  )
  expect(localStorage.getItem('taskto:message:1:/api/workspaces/1/conversations/7')).toBeNull()
  first.unmount()

  render(view(fakeStreams().factory))
  await screen.findByRole('heading', { name: 'Talk 7' })
  expect((screen.getByRole('radio', { name: 'Note' }) as HTMLInputElement).checked).toBe(true)
  expect(box().value).toBe('for the record')
  fireEvent.keyDown(box(), { key: 'Enter' })
  await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
  expect(calls.filter((c) => c.method === 'POST').map((c) => c.path)).toEqual([
    '/api/workspaces/1/conversations/7/notes',
  ])
})

test('a message draft and a note draft are kept apart', async () => {
  claimDrafts(1)
  fakeFetch({ 'GET /api/workspaces/1/conversations/7': { body: conversation } })
  render(view(fakeStreams().factory))
  await screen.findByRole('heading', { name: 'Talk 7' })
  fireEvent.change(box(), { target: { value: 'to the agent' } })
  fireEvent.click(screen.getByRole('radio', { name: 'Note' }))
  expect(box().value).toBe('')
  fireEvent.change(box(), { target: { value: 'for me' } })
  fireEvent.click(screen.getByRole('radio', { name: 'To the agent' }))
  expect(box().value).toBe('to the agent')
})

test('a note that failed to send does not lend its request id to the message sent next', async () => {
  claimDrafts(1)
  let failing = true
  const calls = fakeFetch({
    'GET /api/workspaces/1/conversations/7': { body: conversation },
    'POST /api/workspaces/1/conversations/7/notes': () =>
      failing ? { status: 500, body: { error: 'down' } } : { status: 202, body: { submission_id: 5 } },
    'POST /api/workspaces/1/conversations/7/messages': { status: 202, body: { submission_id: 6 } },
  })
  render(view(fakeStreams().factory))
  await screen.findByRole('heading', { name: 'Talk 7' })
  fireEvent.change(box(), { target: { value: 'to the agent' } })
  fireEvent.click(screen.getByRole('radio', { name: 'Note' }))
  fireEvent.change(box(), { target: { value: 'for me' } })
  fireEvent.keyDown(box(), { key: 'Enter' })
  await waitFor(() => expect(calls.filter((c) => c.method === 'POST')).toHaveLength(1))
  await screen.findByRole('alert')

  failing = false
  fireEvent.click(screen.getByRole('radio', { name: 'To the agent' }))
  expect(box().value).toBe('to the agent')
  fireEvent.keyDown(box(), { key: 'Enter' })
  await waitFor(() => expect(calls.filter((c) => c.method === 'POST')).toHaveLength(2))
  const [note, message] = calls.filter((c) => c.method === 'POST')
  expect(message.path).toBe('/api/workspaces/1/conversations/7/messages')
  expect((message.body as { request_id: string }).request_id).not.toBe((note.body as { request_id: string }).request_id)
})

test('a note sent while the mode changes clears the note draft and leaves the message draft', async () => {
  claimDrafts(1)
  let release: () => void = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  const calls = fakeFetch({
    'GET /api/workspaces/1/conversations/7': { body: conversation },
    'POST /api/workspaces/1/conversations/7/notes': async () => {
      await held
      return { status: 202, body: { submission_id: 5 } }
    },
  })
  render(view(fakeStreams().factory))
  await screen.findByRole('heading', { name: 'Talk 7' })
  fireEvent.change(box(), { target: { value: 'to the agent' } })
  fireEvent.click(screen.getByRole('radio', { name: 'Note' }))
  fireEvent.change(box(), { target: { value: 'for me' } })
  fireEvent.keyDown(box(), { key: 'Enter' })
  await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))

  fireEvent.click(screen.getByRole('radio', { name: 'To the agent' }))
  await act(async () => {
    release()
    await held
  })
  await waitFor(() => expect(localStorage.getItem('taskto:note:1:/api/workspaces/1/conversations/7')).toBeNull())
  expect(box().value).toBe('to the agent')
  fireEvent.click(screen.getByRole('radio', { name: 'Note' }))
  expect(box().value).toBe('')
})

test('what is typed while a note is being sent stays in the box', async () => {
  claimDrafts(1)
  let release: () => void = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  const calls = fakeFetch({
    'GET /api/workspaces/1/conversations/7': { body: conversation },
    'POST /api/workspaces/1/conversations/7/notes': async () => {
      await held
      return { status: 202, body: { submission_id: 5 } }
    },
  })
  render(view(fakeStreams().factory))
  await screen.findByRole('heading', { name: 'Talk 7' })
  fireEvent.click(screen.getByRole('radio', { name: 'Note' }))
  fireEvent.change(box(), { target: { value: 'first' } })
  fireEvent.keyDown(box(), { key: 'Enter' })
  await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))

  fireEvent.change(box(), { target: { value: 'first, and more' } })
  await act(async () => {
    release()
    await held
  })
  await waitFor(() => expect(screen.getByRole('button', { name: 'Add note' })).toBeTruthy())
  expect(box().value).toBe('first, and more')
})
