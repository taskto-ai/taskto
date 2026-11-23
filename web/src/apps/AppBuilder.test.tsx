// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import afterAdd from '../../../internal/apps/testdata/expenses.after-add.json'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { parseRoute, usePath } from '../router'
import { type Call, fakeFetch, fakeStreams } from '../test/fakes'
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
// What the agent saved: a draft nobody has published yet, in space 2.
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
})

// The agent called create_app, and the server answered with the app.
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
// What was sent to the agent, as the conversation's messages route took it.
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

// The builder, in conversation 9, as the page is at this URL.
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
  window.localStorage.setItem('taskto:builder:1:9', JSON.stringify({ space: 2, ...(saved ? { app: 1 } : {}) }))
  window.history.replaceState(null, '', '/w/1/apps/build-9')
  render(<Routed />)
  await waitFor(() => expect(streams.sources).toHaveLength(1))
  act(() => streams.sources[0].open())
}

const panel = (name: 'build' | 'preview') => document.getElementById(`tt-builder-panel-${name}`) as HTMLElement

beforeEach(() => {
  window.history.replaceState(null, '', '/w/1/apps')
  streams = fakeStreams()
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
  const priv = screen.getByRole('radio', { name: 'In my private space' }) as HTMLInputElement
  expect(priv.checked).toBe(true)
  // The start needs a description.
  expect((screen.getByRole('button', { name: 'Start building' }) as HTMLButtonElement).disabled).toBe(true)
  fireEvent.change(what, { target: { value: 'An expense tracker' } })
  fireEvent.click(screen.getByRole('button', { name: 'Start building' }))

  await waitFor(() => expect(window.location.pathname).toBe('/w/1/apps/build-9'))
  const content = sentToAgent(calls)
  expect(content).toContain('An expense tracker')
  expect(content).toContain('my private space (space id 2)')
  expect(content).toMatch(/Don’t publish it/)
  // Where it was started is kept, and the builder opens on the conversation.
  expect(JSON.parse(window.localStorage.getItem('taskto:builder:1:9') ?? '{}')).toEqual({ space: 2 })
  await screen.findByRole('tablist', { name: 'Builder view' })
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
  expect(JSON.parse(window.localStorage.getItem('taskto:builder:1:9') ?? '{}')).toEqual({ space: 3 })
})

test('on a phone, Build and Preview switch without losing the message or the preview, and focus follows', async () => {
  narrowScreen(true)
  const calls = builderRoutes()
  await openBuilder(false)

  // Build is showing; Preview is out of reach, and says there is nothing yet.
  const tabs = screen.getByRole('tablist', { name: 'Builder view' })
  const build = within(tabs).getByRole('tab', { name: 'Build' })
  expect(build.getAttribute('aria-selected')).toBe('true')
  expect(panel('preview').hasAttribute('inert')).toBe(true)
  expect(panel('build').hasAttribute('inert')).toBe(false)

  // The person starts a message the agent hasn't got yet.
  const box = (await screen.findByLabelText('Message')) as HTMLTextAreaElement
  fireEvent.change(box, { target: { value: 'Also add a category' } })

  // The agent saves the app: the Preview tab says it changed, without moving the person.
  act(() => streams.sources[0].send({ type: 'value', value: { entries: savedEntries(1, app()) } }))
  const preview = within(tabs).getByRole('tab', { name: /Preview/ })
  await within(tabs).findByText('(updated)')
  expect(build.getAttribute('aria-selected')).toBe('true')

  fireEvent.click(preview)
  expect(preview.getAttribute('aria-selected')).toBe('true')
  expect(panel('build').hasAttribute('inert')).toBe(true)
  expect(panel('preview').hasAttribute('inert')).toBe(false)
  await waitFor(() => expect(document.activeElement).toBe(panel('preview')))
  expect(within(tabs).queryByText('(updated)')).toBeNull()

  // It is a preview, and says so: sample data, nothing real changes.
  const stage = await within(panel('preview')).findByText('Preview · sample data')
  expect(stage).toBeTruthy()
  await within(panel('preview')).findByText('Coffee')
  expect(within(panel('preview')).getByText(/change nothing real/)).toBeTruthy()

  // Try it: the button runs on the sample rows, never on the app's own actions.
  fireEvent.click(within(panel('preview')).getByRole('button', { name: 'Delete' }))
  await waitFor(() => expect(previews(calls).some((c) => (c.body as { action?: unknown }).action)).toBe(true))
  expect(calls.some((c) => c.path.endsWith('/actions'))).toBe(false)
  expect(calls.some((c) => c.path.includes('/records'))).toBe(false)

  // Back to Build: the message is where it was, and the preview wasn't drawn again meanwhile.
  const drawn = previews(calls).length
  fireEvent.click(build)
  await waitFor(() => expect(document.activeElement).toBe(panel('build')))
  expect((screen.getByLabelText('Message') as HTMLTextAreaElement).value).toBe('Also add a category')
  fireEvent.click(preview)
  expect(previews(calls)).toHaveLength(drawn)
  await within(panel('preview')).findByText('Preview · sample data')
})

test('arrow keys move between the panes', async () => {
  narrowScreen(true)
  builderRoutes()
  await openBuilder()
  const tabs = await screen.findByRole('tablist', { name: 'Builder view' })
  const build = within(tabs).getByRole('tab', { name: 'Build' })
  const preview = within(tabs).getByRole('tab', { name: 'Preview' })
  fireEvent.keyDown(build, { key: 'ArrowRight' })
  expect(preview.getAttribute('aria-selected')).toBe('true')
  expect(document.activeElement).toBe(preview)
  fireEvent.keyDown(preview, { key: 'ArrowLeft' })
  expect(build.getAttribute('aria-selected')).toBe('true')
})

test('on a wide screen the conversation is beside the preview, and there is no switch', async () => {
  narrowScreen(false)
  builderRoutes()
  await openBuilder()
  expect(screen.queryByRole('tablist')).toBeNull()
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
  fireEvent.click(screen.getByRole('tab', { name: 'Preview' }))
  await within(panel('preview')).findByText('Preview · sample data')

  // Leave for the list of apps and come back to the same address.
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
  // The same app is shown, on the pane that was showing, and the message is still there.
  await within(panel('preview')).findByText('Preview · sample data')
  expect(again.some((c) => c.method === 'GET' && c.path === apps)).toBe(true)
  expect(screen.getByRole('tab', { name: 'Preview' }).getAttribute('aria-selected')).toBe('true')
  fireEvent.click(screen.getByRole('tab', { name: 'Build' }))
  expect((screen.getByLabelText('Message') as HTMLTextAreaElement).value).toBe('Add a category field')
  expect(calls.length).toBeGreaterThan(0)

  // Sending it forgets it.
  fireEvent.keyDown(screen.getByLabelText('Message'), { key: 'Enter' })
  await waitFor(() => expect((screen.getByLabelText('Message') as HTMLTextAreaElement).value).toBe(''))
  expect(window.localStorage.getItem('taskto:message:/api/workspaces/1/conversations/9')).toBeNull()
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
  fireEvent.click(screen.getByRole('tab', { name: 'Preview' }))
  const section = await within(panel('preview')).findByRole('region', { name: 'Publish' })
  // Who can use it, and what it can read and change, in plain words.
  expect(within(section).getByText('Only you: it is in your private space.')).toBeTruthy()
  expect(
    within(section).getByText('It reads and keeps records with these fields: description (text) and amount (number).'),
  ).toBeTruthy()
  expect(within(section).getByText('Add records.')).toBeTruthy()
  expect(within(section).getByText('Delete records (a deleted record can be restored).')).toBeTruthy()
  expect(within(section).queryByText('Change records.')).toBeNull()

  // One tap.
  fireEvent.click(within(section).getByRole('button', { name: 'Publish' }))
  await within(section).findByText('Published as release 1. It is live in your private space.')
  const post = calls.find((c) => c.path === `${apps}/publish`)
  expect(post?.body).toEqual({ version: 1, acknowledge: [] })
  // The live release now exists; the preview toggles to it, labelled Live.
  fireEvent.click(await within(panel('preview')).findByRole('button', { name: 'Back to the live release' }))
  await within(panel('preview')).findByText('Live · release 1')
  expect(within(panel('preview')).getByText(/Its buttons change real data/)).toBeTruthy()
})

test('a shared app needs the publisher role, and publishing it names who it goes to first', async () => {
  narrowScreen(true)
  window.localStorage.setItem('taskto:builder:1:9', JSON.stringify({ space: 3, app: 4 }))
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
  fireEvent.click(screen.getByRole('tab', { name: 'Preview' }))
  const section = await within(panel('preview')).findByRole('region', { name: 'Publish' })
  // Who has a role, by name; and a builder without the publisher role can't publish.
  await within(section).findByText('Ann: Builder, Publisher')
  expect(within(section).getByText('Everyone else: User')).toBeTruthy()
  expect(within(section).getByText(/publishing it needs the publisher role in this app/)).toBeTruthy()
  expect(within(section).queryByRole('button', { name: /ublish/ })).toBeNull()

  // With the role, it takes a second step that says who it goes to.
  access = { ...access, publish: true, roles: ['builder', 'publisher'] }
  cleanup()
  streams = fakeStreams()
  render(<Routed />)
  await waitFor(() => expect(streams.sources).toHaveLength(1))
  fireEvent.click(screen.getByRole('tab', { name: 'Preview' }))
  const again = await within(panel('preview')).findByRole('region', { name: 'Publish' })
  fireEvent.click(await within(again).findByRole('button', { name: 'Review and publish…' }))
  expect(calls.some((c) => c.path === `${base}/publish`)).toBe(false)
  expect(within(again).getByText(/live for everyone in Team who may use it/)).toBeTruthy()
  fireEvent.click(within(again).getByRole('button', { name: 'Publish for Team' }))
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
  fireEvent.click(screen.getByRole('tab', { name: 'Preview' }))
  const section = await within(panel('preview')).findByRole('region', { name: 'Publish' })
  fireEvent.click(await within(section).findByRole('button', { name: 'Publish' }))
  const notice = await within(section).findByRole('group', {
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
  fireEvent.click(screen.getByRole('tab', { name: 'Preview' }))
  const section = await within(panel('preview')).findByRole('region', { name: 'Publish' })
  fireEvent.click(await within(section).findByRole('button', { name: 'Publish' }))
  const alert = await within(section).findByRole('alert')
  expect(alert.textContent).toBe('You can’t publish this app: that needs the publisher role in it.')
})

test('someone who can’t change the app is told so instead of being shown its draft', async () => {
  narrowScreen(false)
  window.localStorage.setItem('taskto:builder:1:9', JSON.stringify({ space: 2, app: 1 }))
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
  // The space is the app's: there is nothing to choose.
  expect(screen.queryByRole('radio')).toBeNull()
  fireEvent.change(screen.getByLabelText('What should change?'), { target: { value: 'Add a notes field' } })
  fireEvent.click(screen.getByRole('button', { name: 'Start editing' }))
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/apps/build-9'))
  const content = sentToAgent(calls)
  expect(content).toContain('Add a notes field')
  expect(content).toContain('app id 1, in space id 2')
  expect(content).toMatch(/Change its draft only/)
  expect(JSON.parse(window.localStorage.getItem('taskto:builder:1:9') ?? '{}')).toEqual({ space: 2, app: 1 })
  // Nothing was published or changed by asking.
  expect(calls.some((c) => c.path.endsWith('/publish') || (c.method === 'PATCH' && c.path === apps))).toBe(false)
  // The builder opens on the app: its draft is previewed, and the live release is one tap away.
  await screen.findByRole('tablist', { name: 'Builder view' })
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
