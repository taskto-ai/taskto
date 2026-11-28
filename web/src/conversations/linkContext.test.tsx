// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { fakeFetch, fakeStreams } from '../test/fakes'
import { forgetDrafts } from '../uiState'
import { ConversationView } from './ConversationView'
import { Message } from './Message'
import { transcript } from './transcript'

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
  forgetDrafts()
  vi.unstubAllGlobals()
})

function view(id: number) {
  return (
    <ConversationView
      workspace={1}
      id={id}
      backHref="/w/1/conversations"
      onChanged={() => {}}
      streams={fakeStreams().factory}
    />
  )
}

const ref = (id: number) => `#${id}`

const box = () => screen.getByLabelText('Message') as HTMLTextAreaElement

test('a user message with attached conversations shows what was typed and a chip for each', () => {
  const { messages } = transcript({
    entries: [
      {
        id: 1,
        kind: 'taskto.input',
        data: {
          v: 1,
          author: 3,
          text: 'continue from there',
          attached: [
            { conversation: 12, title: 'Earlier' },
            { conversation: 13, title: '' },
          ],
        },
        model: [
          {
            role: 'user',
            content: 'continue from there\n\n---\nContext from the conversation "Earlier" (12): long hidden text',
          },
        ],
      },
    ],
  })
  render(<Message message={messages[0]} workspace={1} apps={new Set()} />)

  expect(screen.getByText('continue from there')).toBeTruthy()
  expect(screen.queryByText(/long hidden text/)).toBeNull()
  const chips = within(screen.getByRole('list', { name: 'Conversations attached to this message' })).getAllByRole(
    'link',
  )
  expect(chips.map((chip) => [chip.textContent, chip.getAttribute('href')])).toEqual([
    ['Attached: Earlier', '/w/1/conversations/12'],
    [`Attached: ${ref(13)}`, '/w/1/conversations/13'],
  ])
})

test('a user message with nothing attached shows no chips', () => {
  render(<Message message={{ key: 'a', role: 'user', text: 'hello', tools: [] }} workspace={1} apps={new Set()} />)
  expect(screen.queryByRole('list')).toBeNull()
})

test('skipped links are named in a notice after the send, and the notice goes away with the next send', async () => {
  let reply: unknown = {
    submission_id: 5,
    skipped: [
      { conversation: 12, reason: 'unavailable' },
      { conversation: 13, reason: 'too_large' },
    ],
  }
  fakeFetch({
    'GET /api/workspaces/1/conversations/7': { body: conversation(7) },
    'POST /api/workspaces/1/conversations/7/messages': () => ({ status: 202, body: reply }),
  })
  render(view(7))
  await screen.findByRole('heading', { name: 'Talk 7' })

  fireEvent.change(box(), { target: { value: 'from /w/1/conversations/12 and /w/1/conversations/13' } })
  fireEvent.click(screen.getByRole('button', { name: 'Send' }))

  await screen.findByText(
    `The agent didn’t get the linked conversation ${ref(12)}: it can’t be read, or it has no text.`,
  )
  expect(
    screen.getByText(
      `The agent didn’t get the linked conversation ${ref(13)}: the message or the linked conversations are too long.`,
    ),
  ).toBeTruthy()

  reply = { submission_id: 6 }
  fireEvent.change(box(), { target: { value: 'next' } })
  fireEvent.click(screen.getByRole('button', { name: 'Send' }))
  await waitFor(() => expect(screen.queryByText(/didn’t get the linked conversation/)).toBeNull())
})

test('a notice for one conversation never shows in another', async () => {
  let release: (() => void) | undefined
  fakeFetch({
    'GET /api/workspaces/1/conversations/7': { body: conversation(7) },
    'GET /api/workspaces/1/conversations/8': { body: conversation(8) },
    'POST /api/workspaces/1/conversations/7/messages': () =>
      new Promise((done) => {
        release = () =>
          done({ status: 202, body: { submission_id: 5, skipped: [{ conversation: 12, reason: 'unavailable' }] } })
      }),
  })
  const { rerender } = render(view(7))
  await screen.findByRole('heading', { name: 'Talk 7' })
  fireEvent.change(box(), { target: { value: 'from /w/1/conversations/12' } })
  fireEvent.click(screen.getByRole('button', { name: 'Send' }))
  await waitFor(() => expect(release).toBeDefined())

  rerender(view(8))
  await screen.findByRole('heading', { name: 'Talk 8' })
  release?.()
  await waitFor(() => expect(box().value).toBe(''))
  expect(screen.queryByText(/didn’t get the linked conversation/)).toBeNull()
})
