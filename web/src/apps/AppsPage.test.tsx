// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import afterAdd from '../../../internal/apps/testdata/expenses.after-add.json'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { parseRoute, usePath } from '../router'
import { fakeFetch } from '../test/fakes'
import { AppsPage } from './AppsPage'
import { forgetOperations } from './operations'

const spaces = {
  spaces: [
    { id: 2, workspace_id: 1, kind: 'private', name: 'Private', archived_at: null, role: 'owner', access: 'write' },
    { id: 3, workspace_id: 1, kind: 'shared', name: 'Team', archived_at: null, role: 'viewer', access: 'read' },
  ],
}
// What a space's editor may do in an app that has no roles set: everything, over all records.
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
// Release 1, published by Ann, which needs nothing this client lacks.
const release = (over: Record<string, unknown> = {}) => ({
  number: 1,
  definition_version: 1,
  requires: ['component.Column', 'component.Text'],
  published_at: '2026-11-18T10:00:00.000Z',
  published_by: { id: 1, name: 'Ann', agent: false },
  live: true,
  live_since: '2026-11-18T10:00:00.000Z',
  ...over,
})
const draft = { definition_version: 1, requires: ['component.Column', 'component.Text'] }
const app = (over: Record<string, unknown>) => ({
  id: 1,
  space_id: 2,
  name: 'Expenses',
  archived_at: null,
  version: 1,
  rows: 1,
  access: everything,
  live: release(),
  unpublished_changes: false,
  draft,
  ...over,
})

// The page as the shell draws it, following the route.
function Routed() {
  const route = parseRoute(usePath())
  return route ? <AppsPage workspace={route.workspace} open={route.item} /> : null
}

beforeEach(() => window.history.replaceState(null, '', '/w/1/apps'))
afterEach(() => {
  cleanup()
  forgetOperations()
  vi.unstubAllGlobals()
})

test("lists every space's apps, and changes only the ones the user may", async () => {
  let expenses = app({})
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/spaces/2/apps?archived=true': () => ({ body: { apps: [expenses] } }),
    'GET /api/workspaces/1/spaces/3/apps?archived=true': {
      body: { apps: [app({ id: 7, space_id: 3, name: 'Team tracker', rows: 3 })] },
    },
    'PATCH /api/workspaces/1/spaces/2/apps/1': (call) => {
      const body = call.body as { version: number; name?: string; archived?: boolean }
      if (body.version !== expenses.version) return { status: 409, body: { error: 'it changed since you read it' } }
      expenses = app({
        ...expenses,
        version: expenses.version + 1,
        ...(body.name ? { name: body.name } : {}),
        ...(body.archived === undefined ? {} : { archived_at: body.archived ? '2026-11-18T00:00:00.000Z' : null }),
      })
      return { body: expenses }
    },
  })
  render(<Routed />)

  const mine = await screen.findByRole('region', { name: 'Private' })
  const team = screen.getByRole('region', { name: 'Team' })
  await within(team).findByText('Team tracker')
  expect(within(team).getByText(/read-only/)).toBeTruthy()
  expect(within(team).getByText('3 rows')).toBeTruthy()
  // A viewer launches the team's app, and has nothing to change it with.
  expect(within(team).getByRole('link', { name: 'Launch Team tracker' })).toBeTruthy()
  expect(within(team).queryByRole('button', { name: 'Rename' })).toBeNull()
  expect(within(team).queryByRole('button', { name: /Take offline/ })).toBeNull()

  // Rename.
  await within(mine).findByText('Expenses')
  fireEvent.click(within(mine).getByRole('button', { name: 'Rename' }))
  fireEvent.change(within(mine).getByLabelText('Name'), { target: { value: 'Spending' } })
  fireEvent.click(within(mine).getByRole('button', { name: 'Save' }))
  await within(mine).findByText('Spending')
  expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ version: 1, name: 'Spending' })

  // Offline: no launching it, its rows are still counted; then restored.
  fireEvent.click(within(mine).getByRole('button', { name: 'Take offline Spending' }))
  const restore = await within(mine).findByRole('button', { name: 'Restore Spending' })
  expect(within(mine).queryByRole('link', { name: 'Launch Spending' })).toBeNull()
  expect(within(mine).getByText('1 row · offline')).toBeTruthy()
  fireEvent.click(restore)
  await within(mine).findByRole('link', { name: 'Launch Spending' })
  expect(calls.filter((c) => c.method === 'PATCH').map((c) => c.body)).toEqual([
    { version: 1, name: 'Spending' },
    { version: 2, archived: true },
    { version: 3, archived: false },
  ])
})

test('an app is shared by moving it to a space the user may write, with its rows', async () => {
  const withTeam = {
    spaces: [
      ...spaces.spaces,
      { id: 4, workspace_id: 1, kind: 'shared', name: 'Crew', archived_at: null, role: 'editor', access: 'write' },
    ],
  }
  let mine = [app({})]
  let crew: unknown[] = []
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: withTeam },
    'GET /api/workspaces/1/spaces/2/apps?archived=true': () => ({ body: { apps: mine } }),
    'GET /api/workspaces/1/spaces/3/apps?archived=true': { body: { apps: [] } },
    'GET /api/workspaces/1/spaces/4/apps?archived=true': () => ({ body: { apps: crew } }),
    'POST /api/workspaces/1/spaces/2/apps/1/move': () => {
      crew = [app({ space_id: 4, version: 2 })]
      mine = []
      return { body: crew[0] }
    },
  })
  render(<Routed />)

  const private_ = await screen.findByRole('region', { name: 'Private' })
  const select = await within(private_).findByRole('combobox', { name: 'Move Expenses to' })
  // Only the spaces the user may write are offered: not this one, not the team space they view.
  expect(
    within(select)
      .getAllByRole('option')
      .map((o) => o.textContent),
  ).toEqual(['Move to…', 'Crew'])
  fireEvent.change(select, { target: { value: '4' } })
  fireEvent.click(within(private_).getByRole('button', { name: 'Move' }))

  const crewRegion = screen.getByRole('region', { name: 'Crew' })
  await within(crewRegion).findByText('Expenses')
  await within(private_).findByText('No apps here yet.')
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ version: 1, space_id: 4 })
})

test('launching an app draws it with its data', async () => {
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/spaces/2/apps?archived=true': { body: { apps: [app({})] } },
    'GET /api/workspaces/1/spaces/3/apps?archived=true': { body: { apps: [] } },
    'GET /api/workspaces/1/spaces/2/apps/1': { body: app({}) },
    'GET /api/workspaces/1/spaces/2/apps/1/surface?release=1': { body: surface },
  })
  render(<Routed />)
  fireEvent.click(await screen.findByRole('link', { name: 'Launch Expenses' }))
  expect(window.location.pathname).toBe('/w/1/apps/2-1')
  await screen.findByRole('heading', { name: 'Expenses' })
  await screen.findByText('Coffee')
  expect(screen.getByRole('region', { name: 'Expenses' })).toBeTruthy()
  // What is shown is labelled: the live release everyone uses.
  expect(screen.getByText('Live · release 1')).toBeTruthy()
})

test('an offline app is not launched, and its editors can restore it', async () => {
  window.history.replaceState(null, '', '/w/1/apps/2-1')
  let archived: string | null = '2026-11-18T00:00:00.000Z'
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/spaces/2/apps/1': () => ({ body: app({ archived_at: archived, version: 2 }) }),
    'PATCH /api/workspaces/1/spaces/2/apps/1': () => {
      archived = null
      return { body: app({ version: 3 }) }
    },
    'GET /api/workspaces/1/spaces/2/apps/1/surface?release=1': { body: surface },
  })
  render(<Routed />)
  await screen.findByText(/This app is offline/)
  expect(calls.some((c) => c.path.includes('/surface'))).toBe(false)
  fireEvent.click(await screen.findByRole('button', { name: 'Restore' }))
  await screen.findByText('Coffee')
  await waitFor(() => expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ version: 2, archived: false }))
})

test("an app user can launch it but isn't offered what only builders and publishers do", async () => {
  const submitter = {
    ...everything,
    roles: ['user'],
    restricted: true,
    design: false,
    publish: false,
    manage_roles: false,
    read_records: 'own',
    modify_records: 'own',
  }
  const withTeam = {
    spaces: [
      { id: 4, workspace_id: 1, kind: 'shared', name: 'Crew', archived_at: null, role: 'editor', access: 'write' },
    ],
  }
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: withTeam },
    'GET /api/workspaces/1/spaces/4/apps?archived=true': {
      body: { apps: [app({ space_id: 4, access: submitter })] },
    },
  })
  render(<Routed />)
  const crew = await screen.findByRole('region', { name: 'Crew' })
  await within(crew).findByRole('link', { name: 'Launch Expenses' })
  expect(within(crew).queryByRole('button', { name: 'Rename' })).toBeNull()
  expect(within(crew).queryByRole('button', { name: /Take offline/ })).toBeNull()
  expect(within(crew).queryByRole('button', { name: /Roles of/ })).toBeNull()
  expect(within(crew).queryByRole('combobox')).toBeNull()
})

test('a builder sets the roles of members of a shared space', async () => {
  const withTeam = {
    spaces: [
      { id: 4, workspace_id: 1, kind: 'shared', name: 'Crew', archived_at: null, role: 'editor', access: 'write' },
    ],
  }
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: withTeam },
    'GET /api/workspaces/1/spaces/4/apps?archived=true': { body: { apps: [app({ space_id: 4 })] } },
    'GET /api/workspaces/1/spaces/4/apps/1/roles': {
      body: { grants: [{ user_id: null, roles: ['reader'] }], roles: everything.roles },
    },
    'GET /api/workspaces/1/spaces/4/members': {
      body: { members: [{ id: 5, email: 'a@example.com', name: 'Ann', role: 'editor', added_at: '' }] },
    },
    'PUT /api/workspaces/1/spaces/4/apps/1/roles': { body: { grants: [], roles: everything.roles } },
  })
  render(<Routed />)
  const crew = await screen.findByRole('region', { name: 'Crew' })
  fireEvent.click(await within(crew).findByRole('button', { name: 'Roles of Expenses' }))
  const reader = await within(crew).findByRole('checkbox', { name: 'Reader: Everyone else' })
  expect((reader as HTMLInputElement).checked).toBe(true)
  fireEvent.click(within(crew).getByRole('checkbox', { name: 'User: Ann' }))
  fireEvent.click(within(crew).getByRole('button', { name: 'Save' }))
  await within(crew).findByText('Saved.')
  expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({
    grants: [
      { user_id: null, roles: ['reader'] },
      { user_id: 5, roles: ['user'] },
    ],
  })
})

// The roles of a builder who may not publish, and of a publisher.
const builder = {
  ...everything,
  roles: ['builder'],
  restricted: true,
  publish: false,
  submit: false,
  read_records: 'none',
  modify_records: 'none',
}
const publisher = {
  ...everything,
  roles: ['publisher'],
  restricted: true,
  design: false,
  submit: false,
  manage_roles: false,
}
const plainUser = {
  ...everything,
  roles: ['user'],
  restricted: true,
  design: false,
  publish: false,
  manage_roles: false,
  read_records: 'own',
  modify_records: 'own',
}

const crewSpace = (id: number, name: string) => ({
  id,
  workspace_id: 1,
  kind: 'shared',
  name,
  archived_at: null,
  role: 'editor',
  access: 'write',
})

test('shows what is live and what is only a draft, and offers Publish only to those who may publish', async () => {
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces: [crewSpace(4, 'Crew')] } },
    'GET /api/workspaces/1/spaces/4/apps?archived=true': {
      body: {
        apps: [
          app({ id: 1, space_id: 4, name: 'Publishable', live: release({ number: 2 }), unpublished_changes: true }),
          app({ id: 2, space_id: 4, name: 'Builders', access: builder, unpublished_changes: true }),
          app({ id: 3, space_id: 4, name: 'Users', access: plainUser, unpublished_changes: false }),
          app({ id: 4, space_id: 4, name: 'Fresh', live: null, unpublished_changes: true, access: publisher }),
          app({
            id: 5,
            space_id: 4,
            name: 'Only builders see it',
            live: null,
            unpublished_changes: true,
            access: builder,
          }),
          app({ id: 6, space_id: 4, name: 'Hidden draft', live: null, access: plainUser }),
          app({ id: 7, space_id: 4, name: 'Tidy', unpublished_changes: false, access: publisher }),
        ],
      },
    },
  })
  render(<Routed />)
  const crew = await screen.findByRole('region', { name: 'Crew' })
  await within(crew).findByText('Publishable')
  const row = (name: string) => within(crew).getByText(name).closest('li') as HTMLElement

  // Live release 2, with changes that aren't published: the publisher can publish them.
  expect(within(row('Publishable')).getByText('Live · release 2')).toBeTruthy()
  expect(within(row('Publishable')).getByText('Unpublished changes')).toBeTruthy()
  expect(within(row('Publishable')).getByRole('button', { name: 'Publish Publishable' })).toBeTruthy()
  // A builder sees the draft differs, and has no Publish.
  expect(within(row('Builders')).getByText('Unpublished changes')).toBeTruthy()
  expect(within(row('Builders')).queryByRole('button', { name: /^Publish/ })).toBeNull()
  // A user is told nothing of drafts, and has nothing to publish or review.
  expect(within(row('Users')).getByText('Live · release 1')).toBeTruthy()
  expect(within(row('Users')).queryByText('Unpublished changes')).toBeNull()
  expect(within(row('Users')).queryByRole('button', { name: /^Publish|Release history/ })).toBeNull()
  // An app never published: no Launch, a preview of the draft for those who may review it.
  expect(within(row('Fresh')).getByText('Not published')).toBeTruthy()
  expect(within(row('Fresh')).queryByRole('link', { name: /^Launch/ })).toBeNull()
  expect(within(row('Fresh')).getByRole('link', { name: 'Preview the draft of Fresh' })).toBeTruthy()
  expect(within(row('Fresh')).getByRole('button', { name: 'Publish Fresh' })).toBeTruthy()
  expect(within(row('Only builders see it')).queryByRole('button', { name: /^Publish/ })).toBeNull()
  expect(within(row('Hidden draft')).queryByRole('link')).toBeNull()
  // Nothing to publish: no Publish.
  expect(within(row('Tidy')).queryByRole('button', { name: /^Publish/ })).toBeNull()
})

test('publishing asks to acknowledge the schema changes the server names, then publishes with them', async () => {
  const changes = [{ change: 'field_removed', field: 'notes' }]
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces: [crewSpace(4, 'Crew')] } },
    'GET /api/workspaces/1/spaces/4/apps?archived=true': {
      body: { apps: [app({ space_id: 4, unpublished_changes: true, version: 5 })] },
    },
    'POST /api/workspaces/1/spaces/4/apps/1/publish': (call) => {
      const ack = (call.body as { acknowledge: unknown[] }).acknowledge
      if (ack.length === 0) {
        return { status: 409, body: { code: 'app_migration_required', error: 'x', params: { changes } } }
      }
      return { body: app({ space_id: 4, version: 6, live: release({ number: 2 }) }) }
    },
  })
  render(<Routed />)
  const crew = await screen.findByRole('region', { name: 'Crew' })
  fireEvent.click(await within(crew).findByRole('button', { name: 'Publish Expenses' }))

  // Nothing is published yet: the changes are listed, and the person has to say they understand.
  const panel = await within(crew).findByRole('group', { name: 'This publish may change how your app treats its data' })
  expect(within(panel).getByText(/“notes” is removed/)).toBeTruthy()
  const go = within(panel).getByRole('button', { name: 'Publish with these changes' }) as HTMLButtonElement
  expect(go.disabled).toBe(true)
  fireEvent.click(within(panel).getByRole('checkbox', { name: 'I understand these changes' }))
  expect(go.disabled).toBe(false)
  fireEvent.click(go)

  await waitFor(() => expect(within(crew).queryByRole('group')).toBeNull())
  expect(calls.filter((c) => c.method === 'POST').map((c) => c.body)).toEqual([
    { version: 5, acknowledge: [] },
    { version: 5, acknowledge: changes },
  ])
})

test('the release history rolls back, and says why a release is refused', async () => {
  const releases = [
    release({ number: 3, live: true }),
    release({ number: 2, live: false, published_by: null }),
    release({ number: 1, live: false }),
  ]
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces: [crewSpace(4, 'Crew')] } },
    'GET /api/workspaces/1/spaces/4/apps?archived=true': {
      body: { apps: [app({ space_id: 4, version: 7, live: release({ number: 3 }) })] },
    },
    'GET /api/workspaces/1/spaces/4/apps/1/releases': { body: { releases } },
    'POST /api/workspaces/1/spaces/4/apps/1/rollback': (call) =>
      (call.body as { release: number }).release === 2
        ? { status: 409, body: { code: 'app_release_incompatible', error: 'x', params: { fields: ['amount'] } } }
        : { body: app({ space_id: 4 }) },
  })
  render(<Routed />)
  const crew = await screen.findByRole('region', { name: 'Crew' })
  fireEvent.click(await within(crew).findByRole('button', { name: 'Release history of Expenses' }))
  await within(crew).findByText('Release 3')
  expect(within(crew).getByText('Live')).toBeTruthy()
  expect(within(crew).getAllByText(/Published by Ann/)).toHaveLength(2)
  expect(within(crew).getByText(/^Published Nov/)).toBeTruthy() // release 2 has no publisher: its account is gone
  // The live release has nothing to roll back to.
  expect(within(crew).getAllByRole('button', { name: /^Roll back to release/ })).toHaveLength(2)

  fireEvent.click(within(crew).getByRole('button', { name: 'Roll back to release 2' }))
  expect((await within(crew).findByRole('alert')).textContent).toBe(
    'The app’s data doesn’t fit that release, so it can’t go back to it. Fields that don’t fit: amount.',
  )
  fireEvent.click(within(crew).getByRole('button', { name: 'Roll back to release 1' }))
  await waitFor(() => expect(calls.filter((c) => c.method === 'POST')).toHaveLength(2))
  expect(calls.filter((c) => c.method === 'POST').map((c) => c.body)).toEqual([
    { version: 7, release: 2 },
    { version: 7, release: 1 },
  ])
})

test('a publisher without the right to design can roll back, and a builder can read the history only', async () => {
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces: [crewSpace(4, 'Crew')] } },
    'GET /api/workspaces/1/spaces/4/apps?archived=true': {
      body: { apps: [app({ space_id: 4, access: builder })] },
    },
    'GET /api/workspaces/1/spaces/4/apps/1/releases': {
      body: { releases: [release({ number: 2 }), release({ number: 1, live: false })] },
    },
  })
  render(<Routed />)
  const crew = await screen.findByRole('region', { name: 'Crew' })
  fireEvent.click(await within(crew).findByRole('button', { name: 'Release history of Expenses' }))
  await within(crew).findByText('Release 2')
  expect(within(crew).queryByRole('button', { name: /^Roll back/ })).toBeNull()
})

test('a launched app says it is live, and those who may review it can try the draft on sample data', async () => {
  window.history.replaceState(null, '', '/w/1/apps/2-1')
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/spaces/2/apps/1': { body: app({ unpublished_changes: true, live: release({ number: 4 }) }) },
    'GET /api/workspaces/1/spaces/2/apps/1/surface?release=4': { body: surface },
    'POST /api/workspaces/1/spaces/2/apps/1/preview': (call) =>
      (call.body as { action?: unknown }).action
        ? {
            body: {
              messages: afterAdd,
              rows: [],
              effects: [
                {
                  id: 0,
                  operation: '',
                  position: 0,
                  kind: 'create_card',
                  action: 'delete_expense',
                  record: 0,
                  record_revision: 0,
                  status: 'simulated',
                  attempts: 0,
                  next_attempt_at: null,
                  error: null,
                  detail: { title: 'Fix Coffee' },
                  done_at: null,
                  can_retry: false,
                },
              ],
            },
          }
        : { body: { messages: surface, rows: [{ id: 1, revision: 1, data: {} }] } },
    'GET /api/workspaces/1/spaces/2/apps/1/effects?limit=50': { body: { effects: [], active: 0, failed: 0 } },
  })
  render(<Routed />)
  await screen.findByText('Live · release 4')
  await screen.findByText('Coffee')
  expect(screen.getByText(/It has changes that aren’t published yet/)).toBeTruthy()

  fireEvent.click(screen.getByRole('button', { name: 'Preview the draft' }))
  await screen.findByText('Preview · sample data')
  expect(screen.getByText(/preview of changes that aren’t published yet, on sample data/)).toBeTruthy()
  await screen.findByText('Coffee')
  // Its buttons work, on the sample rows the preview sent: they never reach the app's own actions.
  fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
  // …and say what the button would have set going on boards and documents, which changes nothing:
  // the step's own answer, so no other request is made and none reads the app's real records.
  await screen.findByText('Preview: nothing was saved')
  await screen.findByText('Fix Coffee')
  const sent = calls.filter((c) => c.method === 'POST')
  expect(sent.map((c) => c.path)).toEqual(Array(2).fill('/api/workspaces/1/spaces/2/apps/1/preview'))
  expect(sent[0].body).toEqual({}) // the start
  expect(sent[1].body).toMatchObject({ rows: [{ id: 1, revision: 1, data: {} }], action: { name: 'delete_expense' } })
  expect(calls.some((c) => c.path.endsWith('/actions'))).toBe(false)

  fireEvent.click(screen.getByRole('button', { name: 'Back to the live release' }))
  await screen.findByText('Live · release 4')
})

test('someone who may only use an app launches the live release and is offered no draft', async () => {
  window.history.replaceState(null, '', '/w/1/apps/2-1')
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    // The server tells a user nothing of drafts.
    'GET /api/workspaces/1/spaces/2/apps/1': { body: app({ access: plainUser, draft: undefined }) },
    'GET /api/workspaces/1/spaces/2/apps/1/surface?release=1': { body: surface },
  })
  render(<Routed />)
  await screen.findByText('Live · release 1')
  await screen.findByText('Coffee')
  expect(screen.queryByRole('button', { name: /draft/i })).toBeNull()
})

test('an app nobody has published yet shows its draft to its builders, and a note to the rest', async () => {
  window.history.replaceState(null, '', '/w/1/apps/2-1')
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/spaces/2/apps/1': { body: app({ live: null, unpublished_changes: true }) },
    'POST /api/workspaces/1/spaces/2/apps/1/preview': { body: { messages: surface, rows: [] } },
  })
  render(<Routed />)
  await screen.findByText('Preview · sample data')
  await screen.findByText('Coffee')
  cleanup()

  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/spaces/2/apps/1': { body: app({ live: null, access: plainUser, draft: undefined }) },
  })
  render(<Routed />)
  await screen.findByText(/This app isn’t published yet/)
})
