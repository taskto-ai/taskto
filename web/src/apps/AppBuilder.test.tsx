// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import afterAdd from '../../../internal/apps/testdata/expenses.after-add.json'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { navigate, type Place, PlaceContext, parseRoute, usePath } from '../router'
import { beginSession, endSession } from '../session'
import { type Call, fakeFetch, fakeStreams } from '../test/fakes'
import { claimDrafts } from '../uiState'
import { AppsPage } from './AppsPage'

const privateSpace = { id: 2, workspace_id: 1, kind: 'private', name: 'Private', archived_at: null, access: 'write' }
const teamSpace = {
  id: 3,
  workspace_id: 1,
  kind: 'shared',
  name: 'Team',
  archived_at: null,
  role: 'editor',
  access: 'write',
}
const spaces = { spaces: [privateSpace, teamSpace] }
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
const needs = { definition_version: 1, requires: ['component.Column', 'component.Text'] }
const release = (number = 1) => ({
  ...needs,
  number,
  published_at: '2026-11-18T10:00:00.000Z',
  published_by: null,
  live: true,
})
const draft = {
  ...needs,
  collection: {
    fields: [
      { name: 'description', type: 'string' },
      { name: 'amount', type: 'number' },
    ],
  },
  actions: { add_expense: { kind: 'insert' }, delete_expense: { kind: 'delete' } },
}
const app = (over: Record<string, unknown> = {}) => ({
  id: 1,
  space_id: 2,
  name: 'Expenses',
  archived_at: null,
  version: 1,
  rows: 0,
  access: everything,
  live: null,
  unpublished_changes: true,
  draft,
  ...over,
})

const conversation = (id: number) => ({
  id,
  workspace_id: 1,
  title: 'Expenses',
  archived_at: null,
  created_at: '2026-11-18T10:00:00.000Z',
  updated_at: '2026-11-18T10:00:00.000Z',
  role: 'owner',
  owner: { id: 1, name: 'Ana' },
})

function savedEntries(id: number, saved: unknown) {
  return [
    {
      id,
      kind: 'pi.assistant',
      model: [{ role: 'assistant', content: [{ type: 'toolCall', id: `c${id}`, name: 'create_app', arguments: {} }] }],
    },
    {
      id: id + 1,
      kind: 'pi.tool',
      model: [
        {
          role: 'toolResult',
          toolCallId: `c${id}`,
          toolName: 'create_app',
          content: [{ type: 'text', text: JSON.stringify(saved) }],
          isError: false,
        },
      ],
    },
  ]
}

const apps = '/api/workspaces/1/spaces/2/apps/1'
const USER = 7
const SESSION = `taskto:builder:${USER}:1:9`
const MESSAGE = `taskto:message:${USER}:/api/workspaces/1/conversations/9`
const sentToAgent = (calls: Call[]) =>
  String(
    (calls.find((c) => c.path.endsWith('/conversations/9/messages'))?.body as { content?: string } | undefined)
      ?.content,
  )
const previews = (calls: Call[]) => calls.filter((c) => c.path === `${apps}/preview`)

function narrowScreen(narrow: boolean) {
  vi.stubGlobal('matchMedia', (query: string) => ({
    matches: narrow,
    media: query,
    addEventListener: () => undefined,
    removeEventListener: () => undefined,
  }))
}

let streams = fakeStreams()

function Routed() {
  const route = parseRoute(usePath())
  return route ? <AppsPage workspace={route.workspace} open={route.item} streams={streams.factory} /> : null
}

function builderRoutes(extra: Record<string, Parameters<typeof fakeFetch>[0][string]> = {}) {
  return fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/conversations/9': { body: conversation(9) },
    'POST /api/workspaces/1/conversations/9/messages': { status: 202, body: { submission_id: 1 } },
    [`GET ${apps}`]: { body: app() },
    [`POST ${apps}/preview`]: (call) =>
      (call.body as { action?: unknown }).action
        ? { body: { messages: afterAdd, rows: [{ id: 1, revision: 2, data: { description: 'Tea' } }] } }
        : { body: { messages: surface, rows: [{ id: 1, revision: 1, data: { description: 'Coffee' } }] } },
    ...extra,
  })
}

async function openBuilder(saved = true) {
  window.localStorage.setItem(SESSION, JSON.stringify({ space: 2, ...(saved ? { app: 1 } : {}) }))
  window.history.replaceState(null, '', '/w/1/apps/build-9')
  render(<Routed />)
  await waitFor(() => expect(streams.sources).toHaveLength(1))
  act(() => streams.sources[0].open())
}

const panel = (name: 'build' | 'preview') =>
  screen.getByRole('region', { name: name === 'build' ? 'Build' : 'Preview' })
const SWITCH = { name: 'Builder view' }

beforeEach(() => {
  window.history.replaceState(null, '', '/w/1/apps')
  streams = fakeStreams()
  claimDrafts(USER)
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

test('Apps offers to build an app, and to edit one with AI only to those who may change it', async () => {
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/spaces/2/apps?archived=true': {
      body: {
        apps: [
          app({ live: release(), unpublished_changes: false }),
          app({
            id: 2,
            name: 'Read only',
            live: release(),
            access: { ...everything, design: false, publish: false, manage_roles: false },
          }),
        ],
      },
    },
    'GET /api/workspaces/1/spaces/3/apps?archived=true': { body: { apps: [] } },
  })
  render(<Routed />)
  const build = await screen.findByRole('link', { name: 'Build an app' })
  expect(build.getAttribute('href')).toBe('/w/1/apps/build')
  const mine = await screen.findByRole('region', { name: 'Private' })
  await within(mine).findByText('Expenses')
  const edit = within(mine).getByRole('link', { name: 'Edit Expenses with AI' })
  expect(edit.getAttribute('href')).toBe('/w/1/apps/edit-2-1')
  expect(within(mine).queryByRole('link', { name: 'Edit Read only with AI' })).toBeNull()
})

test('a new app starts in the private space unless a shared one is chosen, and the agent is asked to build a draft', async () => {
  narrowScreen(true)
  window.history.replaceState(null, '', '/w/1/apps/build')
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'POST /api/workspaces/1/conversations': { status: 201, body: conversation(9) },
    'POST /api/workspaces/1/conversations/9/messages': { status: 202, body: { submission_id: 1 } },
    'PATCH /api/workspaces/1/conversations/9': { body: conversation(9) },
    'GET /api/workspaces/1/conversations/9': { body: conversation(9) },
  })
  render(<Routed />)
  const what = await screen.findByLabelText('What should the app do?')
  const where = screen.getByRole('combobox', { name: 'Where should it live?' }) as HTMLSelectElement
  expect(where.value).toBe('private')
  expect([...where.options].map((o) => o.textContent)).toEqual(['In my private space', 'In a shared space'])
  expect(screen.queryByRole('radio')).toBeNull()
  expect((screen.getByRole('button', { name: 'Start building' }) as HTMLButtonElement).disabled).toBe(true)
  fireEvent.change(what, { target: { value: 'An expense tracker' } })
  fireEvent.click(screen.getByRole('button', { name: 'Start building' }))

  await waitFor(() => expect(window.location.pathname).toBe('/w/1/apps/build-9'))
  const content = sentToAgent(calls)
  expect(content).toContain('An expense tracker')
  expect(content).toContain('my private space (space id 2)')
  expect(content).toMatch(/Don’t publish it/)
  expect(JSON.parse(window.localStorage.getItem(SESSION) ?? '{}')).toEqual({ space: 2, since: 0 })
  await screen.findByRole('radiogroup', SWITCH)
})

test('starting a build keeps the place’s title: the start screen hands it to the session', async () => {
  window.history.replaceState(null, '', '/w/1/apps/build')
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'POST /api/workspaces/1/conversations': { status: 201, body: conversation(9) },
    'POST /api/workspaces/1/conversations/9/messages': { status: 202, body: { submission_id: 1 } },
    'PATCH /api/workspaces/1/conversations/9': { body: conversation(9) },
    'GET /api/workspaces/1/conversations/9': { body: conversation(9) },
  })
  const titles: (string | null)[] = []
  const at: Place = { id: 'a', path: '/w/1/apps/build', navigate, setTitle: (title) => titles.push(title) }
  render(
    <PlaceContext.Provider value={at}>
      <Routed />
    </PlaceContext.Provider>,
  )
  fireEvent.change(await screen.findByLabelText('What should the app do?'), { target: { value: 'An expense tracker' } })
  expect(titles.at(-1)).toBe('Build an app')
  fireEvent.click(screen.getByRole('button', { name: 'Start building' }))
  await screen.findByRole('region', { name: 'Build' })
  expect(window.location.pathname).toBe('/w/1/apps/build-9')
  expect(titles.at(-1)).toBe('Build an app')
})

test('a shared space is chosen explicitly', async () => {
  window.history.replaceState(null, '', '/w/1/apps/build')
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'POST /api/workspaces/1/conversations': { status: 201, body: conversation(9) },
    'POST /api/workspaces/1/conversations/9/messages': { status: 202, body: { submission_id: 1 } },
    'PATCH /api/workspaces/1/conversations/9': { body: conversation(9) },
    'GET /api/workspaces/1/conversations/9': { body: conversation(9) },
  })
  render(<Routed />)
  fireEvent.change(await screen.findByLabelText('What should the app do?'), { target: { value: 'A team roster' } })
  fireEvent.click(await screen.findByRole('radio', { name: 'In a shared space' }))
  fireEvent.change(screen.getByLabelText('Shared space'), { target: { value: '3' } })
  fireEvent.click(screen.getByRole('button', { name: 'Start building' }))
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/apps/build-9'))
  expect(sentToAgent(calls)).toContain('the shared space “Team” (space id 3)')
  expect(JSON.parse(window.localStorage.getItem(SESSION) ?? '{}')).toEqual({ space: 3, since: 0 })
})

test('on a phone, Build and Preview switch without losing the message or the preview, and say which shows', async () => {
  narrowScreen(true)
  const calls = builderRoutes()
  await openBuilder(false)

  const tabs = screen.getByRole('radiogroup', SWITCH)
  const build = within(tabs).getByRole('radio', { name: 'Build' }) as HTMLInputElement
  expect(build.checked).toBe(true)
  expect(panel('preview').hasAttribute('inert')).toBe(true)
  expect(panel('build').hasAttribute('inert')).toBe(false)

  const box = (await screen.findByLabelText('Message')) as HTMLTextAreaElement
  fireEvent.change(box, { target: { value: 'Also add a category' } })

  act(() => streams.sources[0].send({ type: 'value', value: { entries: savedEntries(1, app()) } }))
  const preview = within(tabs).getByRole('radio', { name: /Preview/ }) as HTMLInputElement
  await within(tabs).findByText('(updated)')
  expect(build.checked).toBe(true)

  fireEvent.click(preview)
  expect(preview.checked).toBe(true)
  expect(panel('build').hasAttribute('inert')).toBe(true)
  expect(panel('preview').hasAttribute('inert')).toBe(false)
  expect(screen.getByText('Showing Preview: the draft of the app.')).toBeTruthy()
  expect(within(tabs).queryByText('(updated)')).toBeNull()

  const stage = await within(panel('preview')).findByText('Preview · sample data')
  expect(stage).toBeTruthy()
  await within(panel('preview')).findByText('Coffee')
  expect(within(panel('preview')).getByText(/change nothing real/)).toBeTruthy()

  fireEvent.click(within(panel('preview')).getByRole('button', { name: 'Delete' }))
  await waitFor(() => expect(previews(calls).some((c) => (c.body as { action?: unknown }).action)).toBe(true))
  expect(calls.some((c) => c.path.endsWith('/actions'))).toBe(false)
  expect(calls.some((c) => c.path.includes('/records'))).toBe(false)

  const drawn = previews(calls).length
  fireEvent.click(build)
  expect(screen.getByText('Showing Build: the conversation with the agent.')).toBeTruthy()
  expect((screen.getByLabelText('Message') as HTMLTextAreaElement).value).toBe('Also add a category')
  fireEvent.click(preview)
  expect(previews(calls)).toHaveLength(drawn)
  await within(panel('preview')).findByText('Preview · sample data')
})

test('the switch is one group of two choices, so the arrow keys move between the panes', async () => {
  narrowScreen(true)
  builderRoutes()
  await openBuilder()
  const tabs = await screen.findByRole('radiogroup', SWITCH)
  const build = within(tabs).getByRole('radio', { name: 'Build' }) as HTMLInputElement
  const preview = within(tabs).getByRole('radio', { name: 'Preview' }) as HTMLInputElement
  expect(build.name).toBe(preview.name)
  fireEvent.click(preview)
  expect(preview.checked).toBe(true)
  expect(panel('build').hasAttribute('inert')).toBe(true)
  fireEvent.click(build)
  expect(build.checked).toBe(true)
  expect(panel('preview').hasAttribute('inert')).toBe(true)
})

test('on a wide screen the conversation is beside the preview, and there is no switch', async () => {
  narrowScreen(false)
  builderRoutes()
  await openBuilder()
  expect(screen.queryByRole('radiogroup', SWITCH)).toBeNull()
  expect(panel('build').hasAttribute('inert')).toBe(false)
  expect(panel('preview').hasAttribute('inert')).toBe(false)
  await within(panel('preview')).findByText('Preview · sample data')
  expect(screen.getByLabelText('Message')).toBeTruthy()
})

test('leaving and reopening keeps the message, the app being built and the pane', async () => {
  narrowScreen(true)
  const calls = builderRoutes()
  await openBuilder()
  fireEvent.change(await screen.findByLabelText('Message'), { target: { value: 'Add a category field' } })
  fireEvent.click(screen.getByRole('radio', { name: 'Preview' }))
  await within(panel('preview')).findByText('Preview · sample data')

  cleanup()
  streams = fakeStreams()
  window.history.replaceState(null, '', '/w/1/apps')
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/spaces/2/apps?archived=true': { body: { apps: [] } },
    'GET /api/workspaces/1/spaces/3/apps?archived=true': { body: { apps: [] } },
  })
  render(<Routed />)
  const resume = await screen.findByRole('link', { name: 'Continue building' })
  expect(resume.getAttribute('href')).toBe('/w/1/apps/build-9')
  cleanup()

  streams = fakeStreams()
  const again = builderRoutes()
  window.history.replaceState(null, '', '/w/1/apps/build-9')
  render(<Routed />)
  await waitFor(() => expect(streams.sources).toHaveLength(1))
  await within(panel('preview')).findByText('Preview · sample data')
  expect(again.some((c) => c.method === 'GET' && c.path === apps)).toBe(true)
  expect((screen.getByRole('radio', { name: 'Preview' }) as HTMLInputElement).checked).toBe(true)
  fireEvent.click(screen.getByRole('radio', { name: 'Build' }))
  expect((screen.getByLabelText('Message') as HTMLTextAreaElement).value).toBe('Add a category field')
  expect(calls.length).toBeGreaterThan(0)

  fireEvent.keyDown(screen.getByLabelText('Message'), { key: 'Enter' })
  await waitFor(() => expect((screen.getByLabelText('Message') as HTMLTextAreaElement).value).toBe(''))
  await waitFor(() => expect(window.localStorage.getItem(MESSAGE)).toBeNull())
})

test('a private app is published in one step, and the panel says who can use it and what it can do', async () => {
  narrowScreen(true)
  let published = false
  const calls = builderRoutes({
    [`GET ${apps}`]: () => ({
      body: published ? app({ version: 2, live: release(), unpublished_changes: false }) : app(),
    }),
    [`POST ${apps}/publish`]: () => {
      published = true
      return { body: app({ version: 2, live: release(), unpublished_changes: false }) }
    },
  })
  await openBuilder()
  fireEvent.click(screen.getByRole('radio', { name: 'Preview' }))
  const section = await within(panel('preview')).findByRole('region', { name: 'Publish' })
  expect(within(section).getByText('Only you: it is in your private space.')).toBeTruthy()
  expect(
    within(section).getByText('It reads and keeps records with these fields: description (text) and amount (number).'),
  ).toBeTruthy()
  expect(within(section).getByText('Add records.')).toBeTruthy()
  expect(within(section).getByText('Delete records (a deleted record can be restored).')).toBeTruthy()
  expect(within(section).queryByText('Change records.')).toBeNull()

  fireEvent.click(within(section).getByRole('button', { name: 'Publish' }))
  await within(section).findByText('Published as release 1. It is live in your private space.')
  const post = calls.find((c) => c.path === `${apps}/publish`)
  expect(post?.body).toEqual({ version: 1, acknowledge: [] })
  await within(panel('preview')).findByText('Live · release 1')
  expect(within(panel('preview')).queryByText('Preview · sample data')).toBeNull()
  expect(within(panel('preview')).queryByRole('button', { name: 'Back to the live release' })).toBeNull()
  expect(within(panel('preview')).queryByRole('button', { name: 'Preview the draft' })).toBeNull()
  expect(within(panel('preview')).getByText(/Nothing is unpublished/)).toBeTruthy()
  expect(within(section).queryByRole('button', { name: /ublish/ })).toBeNull()
})

test('a shared app needs the publisher role, and publishing it names who it goes to first', async () => {
  narrowScreen(true)
  window.localStorage.setItem(SESSION, JSON.stringify({ space: 3, app: 4 }))
  window.history.replaceState(null, '', '/w/1/apps/build-9')
  const base = '/api/workspaces/1/spaces/3/apps/4'
  const shared = (over: Record<string, unknown> = {}) => app({ id: 4, space_id: 3, ...over })
  let access = { ...everything, publish: false, roles: ['builder'], restricted: true, manage_roles: true }
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/conversations/9': { body: conversation(9) },
    [`GET ${base}`]: () => ({ body: shared({ access }) }),
    [`POST ${base}/preview`]: { body: { messages: surface, rows: [] } },
    [`GET ${base}/roles`]: {
      body: {
        grants: [
          { user_id: 7, roles: ['builder', 'publisher'] },
          { user_id: null, roles: ['user'] },
        ],
        roles: [],
      },
    },
    'GET /api/workspaces/1/spaces/3/members': {
      body: { members: [{ id: 7, email: 'ann@example.com', name: 'Ann', role: 'editor', added_at: 'x' }] },
    },
    [`POST ${base}/publish`]: { body: shared() },
  })
  render(<Routed />)
  await waitFor(() => expect(streams.sources).toHaveLength(1))
  fireEvent.click(screen.getByRole('radio', { name: 'Preview' }))
  const section = await within(panel('preview')).findByRole('region', { name: 'Publish' })
  await within(section).findByText('Ann: Builder, Publisher')
  expect(within(section).getByText('Everyone else: User')).toBeTruthy()
  expect(within(section).getByText(/publishing it needs the publisher role in this app/)).toBeTruthy()
  expect(within(section).queryByRole('button', { name: /ublish/ })).toBeNull()

  access = { ...access, publish: true, roles: ['builder', 'publisher'] }
  cleanup()
  streams = fakeStreams()
  render(<Routed />)
  await waitFor(() => expect(streams.sources).toHaveLength(1))
  fireEvent.click(screen.getByRole('radio', { name: 'Preview' }))
  const again = await within(panel('preview')).findByRole('region', { name: 'Publish' })
  fireEvent.click(await within(again).findByRole('button', { name: 'Review and publish…' }))
  expect(calls.some((c) => c.path === `${base}/publish`)).toBe(false)
  const confirm = await screen.findByRole('dialog', { name: 'Publish for Team' })
  expect(within(confirm).getByText(/live for everyone in Team who may use it/)).toBeTruthy()
  fireEvent.click(within(confirm).getByRole('button', { name: 'Publish for Team' }))
  await waitFor(() => expect(calls.some((c) => c.path === `${base}/publish`)).toBe(true))
})

test('a publish that changes the data’s schema asks for the acknowledgement first', async () => {
  narrowScreen(true)
  const changes = [{ change: 'field_removed', field: 'notes' }]
  const calls = builderRoutes({
    [`GET ${apps}`]: { body: app({ live: release(), version: 5 }) },
    [`POST ${apps}/publish`]: (call) =>
      (call.body as { acknowledge: unknown[] }).acknowledge.length === 0
        ? { status: 409, body: { code: 'app_migration_required', error: 'x', params: { changes } } }
        : { body: app({ live: release(2), unpublished_changes: false, version: 6 }) },
  })
  await openBuilder()
  fireEvent.click(screen.getByRole('radio', { name: 'Preview' }))
  const section = await within(panel('preview')).findByRole('region', { name: 'Publish' })
  fireEvent.click(await within(section).findByRole('button', { name: 'Publish' }))
  const notice = await screen.findByRole('dialog', {
    name: 'This publish may change how your app treats its data',
  })
  expect(within(notice).getByText(/“notes” is removed/)).toBeTruthy()
  const go = within(notice).getByRole('button', { name: 'Publish with these changes' }) as HTMLButtonElement
  expect(go.disabled).toBe(true)
  fireEvent.click(within(notice).getByRole('checkbox', { name: 'I understand these changes' }))
  fireEvent.click(go)
  await within(section).findByText(/Published as release 2/)
  expect(calls.filter((c) => c.path === `${apps}/publish`).map((c) => c.body)).toEqual([
    { version: 5, acknowledge: [] },
    { version: 5, acknowledge: changes },
  ])
})

test('permission errors are said in the user’s language, not as the server wrote them', async () => {
  narrowScreen(true)
  builderRoutes({
    [`POST ${apps}/publish`]: {
      status: 403,
      body: {
        error: 'your role in this app doesn’t allow this (publish)',
        code: 'app_forbidden',
        params: { capability: 'publish' },
      },
    },
  })
  await openBuilder()
  fireEvent.click(screen.getByRole('radio', { name: 'Preview' }))
  const section = await within(panel('preview')).findByRole('region', { name: 'Publish' })
  fireEvent.click(await within(section).findByRole('button', { name: 'Publish' }))
  const alert = await within(section).findByRole('alert')
  expect(alert.textContent).toBe('You can’t publish this app: that needs the publisher role in it.')
})

test('someone who can’t change the app is told so instead of being shown its draft', async () => {
  narrowScreen(false)
  window.localStorage.setItem(SESSION, JSON.stringify({ space: 2, app: 1 }))
  window.history.replaceState(null, '', '/w/1/apps/build-9')
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/conversations/9': { body: conversation(9) },
    [`GET ${apps}`]: {
      body: app({
        live: release(),
        draft: undefined,
        unpublished_changes: false,
        access: { ...everything, design: false, publish: false, manage_roles: false },
      }),
    },
  })
  render(<Routed />)
  await waitFor(() => expect(streams.sources).toHaveLength(1))
  await within(panel('preview')).findByText('You can’t change this app: that needs the builder role in it.')
  expect(previews(calls)).toHaveLength(0)
})

test('Edit with AI asks the agent to change the draft of that app, and the live release keeps working', async () => {
  narrowScreen(true)
  window.history.replaceState(null, '', '/w/1/apps/edit-2-1')
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    [`GET ${apps}`]: { body: app({ live: release(), unpublished_changes: false }) },
    'POST /api/workspaces/1/conversations': { status: 201, body: conversation(9) },
    'POST /api/workspaces/1/conversations/9/messages': { status: 202, body: { submission_id: 1 } },
    'PATCH /api/workspaces/1/conversations/9': { body: conversation(9) },
    'GET /api/workspaces/1/conversations/9': { body: conversation(9) },
  })
  render(<Routed />)
  expect(await screen.findByText('Editing Expenses, in Private.')).toBeTruthy()
  expect(screen.queryByRole('radio')).toBeNull()
  fireEvent.change(screen.getByLabelText('What should change?'), { target: { value: 'Add a notes field' } })
  fireEvent.click(screen.getByRole('button', { name: 'Start editing' }))
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/apps/build-9'))
  const content = sentToAgent(calls)
  expect(content).toContain('Add a notes field')
  expect(content).toContain('app id 1, in space id 2')
  expect(content).toMatch(/Change its draft only/)
  expect(JSON.parse(window.localStorage.getItem(SESSION) ?? '{}')).toEqual({ space: 2, since: 0, app: 1 })
  expect(calls.some((c) => c.path.endsWith('/publish') || (c.method === 'PATCH' && c.path === apps))).toBe(false)
  await screen.findByRole('radiogroup', SWITCH)
})

test('Edit with AI is refused to someone who may not design the app', async () => {
  window.history.replaceState(null, '', '/w/1/apps/edit-2-1')
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    [`GET ${apps}`]: { body: app({ live: release(), access: { ...everything, design: false, manage_roles: false } }) },
  })
  render(<Routed />)
  expect((await screen.findByRole('alert')).textContent).toBe(
    'You can’t change this app: that needs the builder role in it.',
  )
  fireEvent.change(screen.getByLabelText('What should change?'), { target: { value: 'x' } })
  expect((screen.getByRole('button', { name: 'Start editing' }) as HTMLButtonElement).disabled).toBe(true)
})

function saveAt(id: number, callId: string, saved: unknown) {
  return savedEntries(id, saved).map((e) => JSON.parse(JSON.stringify(e).replaceAll(`c${id}`, callId)))
}

const getsOf = (calls: Call[], path: string) => calls.filter((c) => c.method === 'GET' && c.path === path)
const NOTHING = 'Nothing to preview yet. Describe the app in Build: the agent’s draft shows here, with sample data.'

test('building in a conversation that has saved other apps opens the builder, and only takes an app saved after', async () => {
  narrowScreen(false)
  window.history.replaceState(null, '', '/w/1/apps/build-9')
  const older = '/api/workspaces/1/spaces/2/apps/5'
  const olderEntries = savedEntries(1, app({ id: 5, name: 'Older' }))
  const route = {
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/conversations/9': { body: conversation(9) },
    'GET /api/workspaces/1/conversations/9/entries': { body: { entries: olderEntries } },
    'POST /api/workspaces/1/conversations/9/messages': { status: 202, body: { submission_id: 1 } },
    [`GET ${older}`]: { body: app({ id: 5, name: 'Older' }) },
    [`GET ${apps}`]: { body: app() },
    [`POST ${older}/preview`]: { body: { messages: surface, rows: [] } },
    [`POST ${apps}/preview`]: { body: { messages: surface, rows: [] } },
  }
  const calls = fakeFetch(route)
  render(<Routed />)
  fireEvent.change(await screen.findByLabelText('What should the app do?'), { target: { value: 'A better tracker' } })
  fireEvent.click(await screen.findByRole('button', { name: 'Start building' }))

  await waitFor(() => expect(streams.sources).toHaveLength(1))
  expect(window.location.pathname).toBe('/w/1/apps/build-9')
  expect(screen.getByLabelText('Message')).toBeTruthy()
  expect(sentToAgent(calls)).toContain('A better tracker')
  act(() => streams.sources[0].open())

  act(() => streams.sources[0].send({ type: 'value', value: { entries: olderEntries } }))
  await within(panel('preview')).findByText(NOTHING)
  expect(getsOf(calls, older)).toHaveLength(0)
  expect(within(panel('preview')).queryByRole('region', { name: 'Publish' })).toBeNull()

  act(() =>
    streams.sources[0].send({ type: 'value', value: { entries: [...olderEntries, ...savedEntries(5, app())] } }),
  )
  await within(panel('preview')).findByText('Preview · sample data')
  expect(getsOf(calls, older)).toHaveLength(0)

  cleanup()
  streams = fakeStreams()
  window.localStorage.setItem(SESSION, JSON.stringify({ space: 2, since: 2 }))
  const again = fakeFetch(route)
  render(<Routed />)
  await waitFor(() => expect(streams.sources).toHaveLength(1))
  act(() => streams.sources[0].open())
  act(() => streams.sources[0].send({ type: 'value', value: { entries: olderEntries } }))
  await within(panel('preview')).findByText(NOTHING)
  expect(getsOf(again, older)).toHaveLength(0)
})

test('a browser that keeps nothing still opens the new build', async () => {
  narrowScreen(true)
  window.history.replaceState(null, '', '/w/1/apps/build')
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'POST /api/workspaces/1/conversations': { status: 201, body: conversation(9) },
    'POST /api/workspaces/1/conversations/9/messages': { status: 202, body: { submission_id: 1 } },
    'PATCH /api/workspaces/1/conversations/9': { body: conversation(9) },
    'GET /api/workspaces/1/conversations/9': { body: conversation(9) },
  })
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
    throw new DOMException('full', 'QuotaExceededError')
  })
  try {
    render(<Routed />)
    fireEvent.change(await screen.findByLabelText('What should the app do?'), { target: { value: 'A tracker' } })
    fireEvent.click(await screen.findByRole('button', { name: 'Start building' }))
    await waitFor(() => expect(window.location.pathname).toBe('/w/1/apps/build-9'))
    await screen.findByRole('radiogroup', SWITCH)
    expect(screen.queryByLabelText('What should the app do?')).toBeNull()
  } finally {
    vi.restoreAllMocks()
  }
})

test('a tab left open by another account never writes the unsent message where the next one reads', async () => {
  narrowScreen(false)
  builderRoutes()
  await openBuilder()
  const box = (await screen.findByLabelText('Message')) as HTMLTextAreaElement
  fireEvent.change(box, { target: { value: 'Alice’s secret plan' } })
  expect(window.localStorage.getItem(MESSAGE)).toBe('Alice’s secret plan')

  window.localStorage.clear()
  window.localStorage.setItem('taskto:drafts-of', '8')

  fireEvent.change(box, { target: { value: 'Alice’s secret plan, continued' } })
  expect(Object.values({ ...window.localStorage }).join('|')).not.toContain('Alice')
})

test('a second save under the same tool call id reads the app again', async () => {
  narrowScreen(false)
  const calls = builderRoutes()
  await openBuilder(false)
  act(() => streams.sources[0].send({ type: 'value', value: { entries: saveAt(1, 'call_0', app()) } }))
  await within(panel('preview')).findByText('Preview · sample data')
  const first = getsOf(calls, apps).length
  act(() =>
    streams.sources[0].send({
      type: 'value',
      value: { entries: [...saveAt(1, 'call_0', app()), ...saveAt(3, 'call_0', app({ version: 2 }))] },
    }),
  )
  await waitFor(() => expect(getsOf(calls, apps).length).toBeGreaterThan(first))
})

test('an app a CLI turn saved fills the preview of a new build', async () => {
  narrowScreen(false)
  const calls = builderRoutes()
  await openBuilder(false)
  expect(getsOf(calls, apps)).toHaveLength(0)
  const cli = [
    {
      id: 1,
      kind: 'pi.assistant',
      model: [
        { role: 'assistant', content: [{ type: 'toolCall', id: 'k', name: 'cli_turn', arguments: { cli: 'claude' } }] },
      ],
    },
    {
      id: 2,
      kind: 'pi.tool',
      model: [
        {
          role: 'toolResult',
          toolCallId: 'k',
          toolName: 'cli_turn',
          content: [{ type: 'text', text: 'Saved the app.' }],
          details: { transcript: '▸ mcp__taskto__create_app', apps: [{ space: 2, app: 1 }] },
        },
      ],
    },
  ]
  act(() => streams.sources[0].send({ type: 'value', value: { entries: cli } }))
  await within(panel('preview')).findByText('Preview · sample data')
  expect(getsOf(calls, apps).length).toBeGreaterThan(0)
})

test('a shared publish confirmation closes when the draft changes, and publishes only what was reviewed again', async () => {
  narrowScreen(false)
  window.localStorage.setItem(SESSION, JSON.stringify({ space: 3, app: 4, since: 0 }))
  window.history.replaceState(null, '', '/w/1/apps/build-9')
  const base = '/api/workspaces/1/spaces/3/apps/4'
  const shared = (over: Record<string, unknown> = {}) => app({ id: 4, space_id: 3, ...over })
  let version = 1
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/conversations/9': { body: conversation(9) },
    [`GET ${base}`]: () => ({ body: shared({ version }) }),
    [`POST ${base}/preview`]: { body: { messages: surface, rows: [] } },
    [`POST ${base}/publish`]: { body: shared({ version: version + 1, live: release(), unpublished_changes: false }) },
  })
  render(<Routed />)
  await waitFor(() => expect(streams.sources).toHaveLength(1))
  act(() => streams.sources[0].open())
  const section = await within(panel('preview')).findByRole('region', { name: 'Publish' })
  fireEvent.click(await within(section).findByRole('button', { name: 'Review and publish…' }))
  expect((await screen.findByRole('dialog', { name: 'Publish for Team' })).textContent).toContain('Team')

  version = 2
  act(() => streams.sources[0].send({ type: 'value', value: { entries: savedEntries(1, shared({ version: 2 })) } }))
  await within(section).findByText(/changed while you were reviewing it/)
  expect(screen.queryByRole('dialog', { name: 'Publish for Team' })).toBeNull()
  expect(calls.some((c) => c.path === `${base}/publish`)).toBe(false)

  fireEvent.click(within(section).getByRole('button', { name: 'Review and publish…' }))
  fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Publish for Team' }))
  await waitFor(() => expect(calls.some((c) => c.path === `${base}/publish`)).toBe(true))
  expect(calls.find((c) => c.path === `${base}/publish`)?.body).toEqual({ version: 2, acknowledge: [] })
})

test('a draft the agent saves for a published app leaves the live form as it is, and says so', async () => {
  narrowScreen(false)
  let saved = false
  const published = app({ version: 2, live: release(), unpublished_changes: false })
  const calls = builderRoutes({
    [`GET ${apps}`]: () => ({ body: saved ? { ...published, version: 3, unpublished_changes: true } : published }),
    [`GET ${apps}/surface?release=1`]: { body: { messages: surface, rows: [] } },
  })
  await openBuilder()
  const stage = panel('preview')
  await within(stage).findByText('Live · release 1')
  const field = (await within(stage).findByLabelText('Description')) as HTMLInputElement
  fireEvent.change(field, { target: { value: 'Half-typed lunch' } })

  saved = true
  act(() => streams.sources[0].send({ type: 'value', value: { entries: savedEntries(1, app({ version: 3 })) } }))
  await within(stage).findByText(/A new draft is ready/)
  expect(within(stage).queryByText('Preview · sample data')).toBeNull()
  expect((within(stage).getByLabelText('Description') as HTMLInputElement).value).toBe('Half-typed lunch')

  fireEvent.click(within(stage).getByRole('button', { name: 'Preview the draft' }))
  await within(stage).findByText('Preview · sample data')
  fireEvent.click(within(stage).getByRole('button', { name: 'Back to the live release' }))
  const back = within(stage).getByLabelText('Description') as HTMLInputElement
  expect(back.value).toBe('Half-typed lunch')
  expect(calls.length).toBeGreaterThan(0)
})

test('the builder’s bar says where the draft stands: never published, unpublished changes, live', async () => {
  narrowScreen(false)
  let shown = app()
  builderRoutes({ [`GET ${apps}`]: () => ({ body: shown }), [`GET ${apps}/surface?release=1`]: { body: surface } })
  await openBuilder()
  const bar = () => screen.getByRole('heading', { name: 'Build an app', level: 1 }).parentElement as HTMLElement
  await within(bar()).findByText('Not published')
  cleanup()

  shown = app({ live: release(), unpublished_changes: true })
  streams = fakeStreams()
  builderRoutes({ [`GET ${apps}`]: () => ({ body: shown }), [`GET ${apps}/surface?release=1`]: { body: surface } })
  await openBuilder()
  await within(bar()).findByText('Unpublished changes')
  cleanup()

  shown = app({ live: release(3), unpublished_changes: false })
  streams = fakeStreams()
  builderRoutes({ [`GET ${apps}`]: () => ({ body: shown }), [`GET ${apps}/surface?release=3`]: { body: surface } })
  await openBuilder()
  await within(bar()).findByText('Live · release 3')
})

test('a publish refused after the acknowledgement keeps the schema dialog, with why; cancelling it gives the focus back', async () => {
  narrowScreen(false)
  const changes = [{ change: 'field_removed', field: 'notes' }]
  let refuse = true
  builderRoutes({
    [`GET ${apps}`]: { body: app({ live: release(), version: 5 }) },
    [`POST ${apps}/surface?release=1`]: { body: surface },
    [`POST ${apps}/publish`]: (call) => {
      if ((call.body as { acknowledge: unknown[] }).acknowledge.length === 0)
        return { status: 409, body: { code: 'app_migration_required', error: 'x', params: { changes } } }
      if (refuse)
        return {
          status: 403,
          body: { error: 'x', code: 'app_forbidden', params: { capability: 'publish' } },
        }
      return { body: app({ live: release(2), unpublished_changes: false, version: 6 }) }
    },
  })
  await openBuilder()
  const section = await within(panel('preview')).findByRole('region', { name: 'Publish' })
  const publish = await within(section).findByRole('button', { name: 'Publish' })
  fireEvent.click(publish)
  const name = 'This publish may change how your app treats its data'
  let dialog = await screen.findByRole('dialog', { name })
  fireEvent.click(within(dialog).getByRole('checkbox', { name: 'I understand these changes' }))
  fireEvent.click(within(dialog).getByRole('button', { name: 'Publish with these changes' }))

  expect((await within(dialog).findByRole('alert')).textContent).toBe(
    'You can’t publish this app: that needs the publisher role in it.',
  )
  expect(screen.getByRole('dialog', { name })).toBe(dialog)
  expect(
    (within(dialog).getByRole('checkbox', { name: 'I understand these changes' }) as HTMLInputElement).checked,
  ).toBe(true)

  fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
  await waitFor(() => expect(screen.queryByRole('dialog', { name })).toBeNull())
  const again = within(section).getByRole('button', { name: 'Publish' })
  await waitFor(() => expect(document.activeElement).toBe(again))

  refuse = false
  fireEvent.click(again)
  dialog = await screen.findByRole('dialog', { name })
  fireEvent.click(within(dialog).getByRole('checkbox', { name: 'I understand these changes' }))
  fireEvent.click(within(dialog).getByRole('button', { name: 'Publish with these changes' }))
  await within(section).findByText(/Published as release 2/)
})

test('on a phone a shared space is chosen with the select, then the shared space', async () => {
  narrowScreen(true)
  window.history.replaceState(null, '', '/w/1/apps/build')
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'POST /api/workspaces/1/conversations': { status: 201, body: conversation(9) },
    'POST /api/workspaces/1/conversations/9/messages': { status: 202, body: { submission_id: 1 } },
    'PATCH /api/workspaces/1/conversations/9': { body: conversation(9) },
    'GET /api/workspaces/1/conversations/9': { body: conversation(9) },
  })
  render(<Routed />)
  fireEvent.change(await screen.findByLabelText('What should the app do?'), { target: { value: 'A team roster' } })
  fireEvent.change(screen.getByRole('combobox', { name: 'Where should it live?' }), { target: { value: 'shared' } })
  expect((screen.getByLabelText('Shared space') as HTMLSelectElement).value).toBe('3')
  fireEvent.click(screen.getByRole('button', { name: 'Start building' }))
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/apps/build-9'))
  expect(sentToAgent(calls)).toContain('the shared space “Team” (space id 3)')
})

const NEW = 'POST /api/workspaces/1/conversations'
const MESSAGES = 'POST /api/workspaces/1/conversations/9/messages'
const TITLE = 'PATCH /api/workspaces/1/conversations/9'
const ENTRIES = 'GET /api/workspaces/1/conversations/9/entries'

async function startHeld(hold: string, page: '/w/1/apps/build' | '/w/1/apps/build-9') {
  narrowScreen(false)
  window.history.replaceState(null, '', page)
  let release: () => void = () => {}
  const answers: Record<string, { status?: number; body?: unknown }> = {
    [NEW]: { status: 201, body: conversation(9) },
    [MESSAGES]: { status: 202, body: { submission_id: 1 } },
    [TITLE]: { body: conversation(9) },
    [ENTRIES]: { body: { entries: [] } },
  }
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/conversations/9': { body: conversation(9) },
    ...answers,
    [hold]: () =>
      new Promise((done) => {
        release = () => done(answers[hold])
      }),
  })
  beginSession(USER)
  const view = render(<Routed />)
  fireEvent.change(await screen.findByLabelText('What should the app do?'), { target: { value: 'Private plans' } })
  fireEvent.click(screen.getByRole('button', { name: 'Start building' }))
  const [method, path] = hold.split(' ')
  await waitFor(() => expect(calls.some((c) => c.method === method && c.path === path)).toBe(true))
  const sent = () => calls.map((c) => `${c.method} ${c.path}`)
  const before = sent().length
  return { calls, view, release: () => act(async () => release()), after: () => sent().slice(before) }
}

for (const [hold, page, ending] of [
  [NEW, '/w/1/apps/build', 'unmount'],
  [NEW, '/w/1/apps/build', 'session'],
  [MESSAGES, '/w/1/apps/build', 'session'],
  [TITLE, '/w/1/apps/build', 'unmount'],
  [ENTRIES, '/w/1/apps/build-9', 'session'],
] as const) {
  test(`a build whose ${hold} answers after the ${ending === 'unmount' ? 'page went' : 'session ended'} goes no further`, async () => {
    const started = await startHeld(hold, page)
    if (ending === 'unmount') started.view.unmount()
    else endSession()
    await started.release()
    await new Promise((r) => setTimeout(r, 20))
    expect(started.after().filter((c) => c.startsWith('POST') || c.startsWith('PATCH'))).toEqual([])
    expect(window.localStorage.getItem(SESSION)).toBeNull()
    expect(window.location.pathname).toBe(page)
  })
}

test('a build whose session changed while it waited leaves its button ready on the page that is still here', async () => {
  const started = await startHeld(NEW, '/w/1/apps/build')
  endSession()
  beginSession(USER)
  await started.release()
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Start building' }).getAttribute('aria-busy')).toBeNull(),
  )
  expect(window.location.pathname).toBe('/w/1/apps/build')
})
