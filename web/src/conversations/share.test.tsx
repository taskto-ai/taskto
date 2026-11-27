// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { fakeFetch, fakeStreams } from '../test/fakes'
import { ConversationsPage } from './ConversationsPage'
import { ConversationView } from './ConversationView'

const owner = { id: 1, name: 'Ana' }

const conversation = (over: Record<string, unknown> = {}) => ({
  id: 7,
  workspace_id: 1,
  title: 'Plans',
  archived_at: null,
  created_at: '2026-11-18T10:00:00.000Z',
  updated_at: '2026-11-18T10:00:00.000Z',
  cli_permissions: 'ask',
  role: 'owner',
  owner,
  ...over,
})

const person = (id: number, name: string, extra: Record<string, unknown> = {}) => ({
  id,
  email: `${name.toLowerCase()}@example.com`,
  name,
  role: 'member',
  joined_at: '2026-11-01T10:00:00.000Z',
  ...extra,
})

const workspaceMembers = {
  members: [person(1, 'Ana'), person(2, 'Bo'), person(3, 'Cy'), person(4, 'Di', { disabled: true })],
}

const MODELS = {
  'GET /api/workspaces/1/models': {
    body: {
      providers: [{ provider: 'script', name: 'Scripted', scope: 'instance', models: [{ id: 's', name: 'S' }] }],
    },
  },
  'GET /api/workspaces/1/conversations/7/model': { body: { provider: 'script', model_id: 's' } },
}

beforeEach(() => window.history.replaceState(null, '', '/w/1/conversations/7'))
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const view = (factory: ReturnType<typeof fakeStreams>['factory']) => (
  <ConversationView
    workspace={1}
    id={7}
    backHref="/w/1/conversations"
    onChanged={() => {}}
    streams={factory}
    embedApps={false}
  />
)

test('the owner shares with a member of the workspace and stops sharing again', async () => {
  let shared: { user: { id: number; name: string }; role: string; added_at: string }[] = [
    { user: { id: 3, name: 'Cy' }, role: 'viewer', added_at: '2026-11-19T10:00:00.000Z' },
  ]
  const calls = fakeFetch({
    ...MODELS,
    'GET /api/workspaces/1/conversations/7': { body: conversation() },
    'GET /api/workspaces/1/conversations/7/members': () => ({ body: { members: shared } }),
    'GET /api/workspaces/1/members': { body: workspaceMembers },
    'PUT /api/workspaces/1/conversations/7/members/2': () => {
      shared = [...shared, { user: { id: 2, name: 'Bo' }, role: 'viewer', added_at: '2026-11-19T11:00:00.000Z' }]
      return { body: shared[1] }
    },
    'DELETE /api/workspaces/1/conversations/7/members/3': () => {
      shared = shared.filter((m) => m.user.id !== 3)
      return { status: 204 }
    },
  })
  render(view(fakeStreams().factory))
  fireEvent.click(await screen.findByRole('button', { name: 'Share' }))
  const dialog = await screen.findByRole('dialog', { name: 'Share this conversation' })

  const people = await within(dialog).findByRole('list', { name: 'People it is shared with' })
  expect(within(people).getByText('Cy')).toBeTruthy()
  const picker = within(dialog).getByLabelText('Add a person') as HTMLSelectElement
  await waitFor(() => expect([...picker.options].map((o) => o.textContent)).toEqual(['Choose a member', 'Bo']))
  expect((within(dialog).getAllByRole('button', { name: 'Share' }).at(-1) as HTMLButtonElement).disabled).toBe(true)

  fireEvent.change(picker, { target: { value: '2' } })
  fireEvent.click(within(dialog).getAllByRole('button', { name: 'Share' }).at(-1) as HTMLElement)
  await waitFor(() => expect(within(dialog).getByText('Bo')).toBeTruthy())
  expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ role: 'viewer' })
  expect(await within(dialog).findByText('Everyone in the workspace can already read it.')).toBeTruthy()

  fireEvent.click(within(dialog).getByRole('button', { name: 'Stop sharing with Cy' }))
  await waitFor(() =>
    expect(within(within(dialog).getByRole('list', { name: 'People it is shared with' })).queryByText('Cy')).toBeNull(),
  )
  expect(calls.some((c) => c.method === 'DELETE' && c.path.endsWith('/members/3'))).toBe(true)
})

test('a refused share says why and the list stays as it was', async () => {
  fakeFetch({
    ...MODELS,
    'GET /api/workspaces/1/conversations/7': { body: conversation() },
    'GET /api/workspaces/1/conversations/7/members': { body: { members: [] } },
    'GET /api/workspaces/1/members': { body: workspaceMembers },
    'PUT /api/workspaces/1/conversations/7/members/2': {
      status: 409,
      body: { error: 'too many', code: 'conversation_members_limit', params: { max: 50 } },
    },
  })
  render(view(fakeStreams().factory))
  fireEvent.click(await screen.findByRole('button', { name: 'Share' }))
  const dialog = await screen.findByRole('dialog', { name: 'Share this conversation' })
  const picker = within(dialog).getByLabelText('Add a person') as HTMLSelectElement
  await waitFor(() => expect(picker.options.length).toBe(3))
  fireEvent.change(picker, { target: { value: '2' } })
  fireEvent.click(within(dialog).getAllByRole('button', { name: 'Share' }).at(-1) as HTMLElement)
  expect(await within(dialog).findByText('A conversation can be shared with at most 50 people.')).toBeTruthy()
  expect(within(dialog).getByText('It isn’t shared with anyone.'.replace('’', "'"))).toBeTruthy()
})

test('a person it is shared with sees it read only: no composer, no rename, no share, and a notice', async () => {
  const calls = fakeFetch({
    ...MODELS,
    'GET /api/workspaces/1/conversations/7': { body: conversation({ role: 'viewer', owner: { id: 5, name: 'Eve' } }) },
  })
  const { sources, factory } = fakeStreams()
  render(view(factory))
  await screen.findByRole('heading', { name: 'Plans' })
  expect(await screen.findByText('Shared by Eve · read only')).toBeTruthy()
  await waitFor(() => expect(sources).toHaveLength(1))
  act(() => sources[0].open())

  expect(screen.queryByLabelText('Message')).toBeNull()
  expect(screen.queryByRole('button', { name: 'Send' })).toBeNull()
  expect(screen.queryByRole('button', { name: 'Share' })).toBeNull()
  expect(screen.queryByRole('button', { name: 'Rename' })).toBeNull()
  expect(screen.queryByRole('button', { name: 'Archive' })).toBeNull()
  expect(screen.queryByLabelText('Model')).toBeNull()
  expect(calls.some((c) => c.path.includes('cli-permissions'))).toBe(false)
  expect(calls.some((c) => c.method !== 'GET')).toBe(false)
})

test('the owner has the composer and no read only notice', async () => {
  fakeFetch({ ...MODELS, 'GET /api/workspaces/1/conversations/7': { body: conversation() } })
  render(view(fakeStreams().factory))
  expect(await screen.findByLabelText('Message')).toBeTruthy()
  expect(screen.queryByText(/read only/)).toBeNull()
  expect(screen.getByRole('button', { name: 'Rename' })).toBeTruthy()
})

test('the list switches to what others have shared, with their names', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [conversation({ id: 1, title: 'Mine' })] } },
    'GET /api/workspaces/1/conversations?shared=true': {
      body: {
        conversations: [conversation({ id: 9, title: 'Theirs', role: 'viewer', owner: { id: 5, name: 'Eve' } })],
      },
    },
  })
  window.history.replaceState(null, '', '/w/1/conversations')
  render(<ConversationsPage workspace={1} />)
  await screen.findByText('Mine')
  expect(screen.queryByText(/Shared by/)).toBeNull()

  fireEvent.click(screen.getByRole('button', { name: 'Shared with me' }))
  await screen.findByText('Theirs')
  expect(screen.queryByText('Mine')).toBeNull()
  expect(screen.getByText(/Shared by Eve/)).toBeTruthy()
  expect(screen.getByRole('heading', { name: 'Shared with me' })).toBeTruthy()
  expect(calls.some((c) => c.path === '/api/workspaces/1/conversations?shared=true')).toBe(true)
})

test('nothing shared says so', async () => {
  fakeFetch({
    'GET /api/workspaces/1/conversations': { body: { conversations: [] } },
    'GET /api/workspaces/1/conversations?shared=true': { body: { conversations: [] } },
  })
  window.history.replaceState(null, '', '/w/1/conversations')
  render(<ConversationsPage workspace={1} />)
  await screen.findByText('No conversations yet.')
  fireEvent.click(screen.getByRole('button', { name: 'Shared with me' }))
  expect(await screen.findByText('Nobody has shared a conversation with you.')).toBeTruthy()
})
