// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import afterAdd from '../../../internal/apps/testdata/expenses.after-add.json'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { PinnedNav } from '../home/PinnedNav'
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
  icon: '',
  archived_at: null,
  version: 1,
  rows: 1,
  access: everything,
  live: release(),
  unpublished_changes: false,
  draft,
  ...over,
})

function Routed() {
  const route = parseRoute(usePath())
  return route ? <AppsPage workspace={route.workspace} open={route.item} /> : null
}

beforeEach(() => window.history.replaceState(null, '', '/w/1/apps'))

async function menuItems(region: HTMLElement, name: string): Promise<string[]> {
  const button = await within(region).findByRole('button', { name: `More actions for ${name}` })
  fireEvent.click(button)
  const items = within(region)
    .getAllByRole('menuitem')
    .map((i) => i.textContent ?? '')
  fireEvent.click(button)
  return items
}

const shown = (els: HTMLElement[]) => els.filter((el) => !el.closest('[hidden]'))

async function choose(region: HTMLElement, name: string, item: string) {
  fireEvent.click(await within(region).findByRole('button', { name: `More actions for ${name}` }))
  fireEvent.click(within(region).getByRole('menuitem', { name: item }))
}
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
  expect(within(team).getByText(/^3 rows · published/)).toBeTruthy()
  expect(within(team).getByRole('link', { name: 'Open Team tracker' })).toBeTruthy()
  expect(within(team).queryByRole('link', { name: /with AI/ })).toBeNull()
  expect(await menuItems(team, 'Team tracker')).toEqual(['Duplicate as template'])

  await within(mine).findByText('Expenses')
  expect(within(mine).getByText('Live · release 1')).toBeTruthy()
  expect(await menuItems(mine, 'Expenses')).toEqual([
    'History',
    'Rename',
    'Change icon',
    'Duplicate as template',
    'Take offline',
  ])

  await choose(mine, 'Expenses', 'Rename')
  const rename = await screen.findByRole('dialog', { name: 'Rename Expenses' })
  fireEvent.change(within(rename).getByLabelText('Name'), { target: { value: 'Spending' } })
  fireEvent.click(within(rename).getByRole('button', { name: 'Save' }))
  await within(mine).findByText('Spending')
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ version: 1, name: 'Spending' })

  await choose(mine, 'Spending', 'Take offline')
  let ask = await screen.findByRole('alertdialog', { name: 'Take Spending offline?' })
  fireEvent.click(within(ask).getByRole('button', { name: 'Cancel' }))
  await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
  expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(1)

  await choose(mine, 'Spending', 'Take offline')
  ask = await screen.findByRole('alertdialog', { name: 'Take Spending offline?' })
  fireEvent.click(within(ask).getByRole('button', { name: 'Take offline' }))
  const restore = await within(mine).findByRole('button', { name: 'Restore Spending' })
  expect(within(mine).queryByRole('link', { name: 'Open Spending' })).toBeNull()
  expect(within(mine).getByText('Offline')).toBeTruthy()
  expect(within(mine).getByText(/^1 row/)).toBeTruthy()
  fireEvent.click(restore)
  await within(mine).findByRole('link', { name: 'Open Spending' })
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
  await choose(private_, 'Expenses', 'Move to a space…')
  const dialog = await screen.findByRole('dialog', { name: 'Move Expenses to a space' })
  const select = within(dialog).getByRole('combobox', { name: 'Move Expenses to' })
  expect(
    within(select)
      .getAllByRole('option')
      .map((o) => o.textContent),
  ).toEqual(['Move to…', 'Crew'])
  fireEvent.change(select, { target: { value: '4' } })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Move' }))

  const crewRegion = screen.getByRole('region', { name: 'Crew' })
  await within(crewRegion).findByText('Expenses')
  await within(private_).findByText('No apps in Private yet')
  expect(within(private_).getByRole('link', { name: 'Build an app' })).toBeTruthy()
  expect(within(private_).getByRole('link', { name: 'Start from a template' })).toBeTruthy()
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
  fireEvent.click(await screen.findByRole('link', { name: 'Open Expenses' }))
  expect(window.location.pathname).toBe('/w/1/apps/2-1')
  await screen.findByRole('heading', { name: 'Expenses' })
  await screen.findByText('Coffee')
  expect(screen.getByRole('region', { name: 'Expenses' })).toBeTruthy()
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
  await within(crew).findByRole('link', { name: 'Open Expenses' })
  expect(within(crew).queryByRole('link', { name: /with AI/ })).toBeNull()
  expect(await menuItems(crew, 'Expenses')).toEqual(['Duplicate as template'])
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
  await choose(crew, 'Expenses', 'Roles')
  const dialog = await screen.findByRole('dialog', { name: 'Roles of Expenses' })
  const table = await within(dialog).findByRole('table', { name: 'Roles of each member in this app' })
  expect(
    within(table)
      .getAllByRole('columnheader')
      .map((h) => h.textContent),
  ).toEqual(['Member', 'Builder', 'Publisher', 'User', 'Data manager', 'Reader'])
  expect(
    within(table)
      .getAllByRole('rowheader')
      .map((h) => h.textContent),
  ).toEqual(['Everyone else', 'Ann'])
  const reader = within(table).getByRole('checkbox', { name: 'Reader: Everyone else' })
  expect((reader as HTMLInputElement).checked).toBe(true)
  fireEvent.click(within(table).getByRole('checkbox', { name: 'User: Ann' }))
  fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }))
  await within(dialog).findByText('Saved.')
  expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({
    grants: [
      { user_id: null, roles: ['reader'] },
      { user_id: 5, roles: ['user'] },
    ],
  })
})

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

  const items = (name: string) => menuItems(row(name), name)

  expect(within(row('Publishable')).getByText('Live · release 2')).toBeTruthy()
  expect(within(row('Publishable')).getByText('Unpublished changes')).toBeTruthy()
  expect(await items('Publishable')).toContain('Publish…')
  expect(within(row('Builders')).getByText('Unpublished changes')).toBeTruthy()
  expect(await items('Builders')).not.toContain('Publish…')
  expect(within(row('Users')).getByText('Live · release 1')).toBeTruthy()
  expect(within(row('Users')).queryByText('Unpublished changes')).toBeNull()
  expect(await items('Users')).toEqual(['Duplicate as template'])
  expect(within(row('Fresh')).getByText('Not published')).toBeTruthy()
  expect(within(row('Fresh')).queryByRole('link', { name: /^Open/ })).toBeNull()
  expect(within(row('Fresh')).getByRole('link', { name: 'Preview the draft of Fresh' })).toBeTruthy()
  expect(await items('Fresh')).toContain('Publish…')
  expect(await items('Only builders see it')).not.toContain('Publish…')
  expect(within(row('Hidden draft')).queryByRole('link')).toBeNull()
  expect(await items('Tidy')).not.toContain('Publish…')
})

const withEffects = (over: Record<string, unknown> = {}) =>
  app({
    space_id: 4,
    unpublished_changes: true,
    version: 5,
    draft: {
      ...draft,
      actions: {
        submit: {
          kind: 'insert',
          effects: [{ kind: 'create_card', name: 'fix', space_id: 7, board_id: 12 }],
        },
      },
    },
    effect_targets: [{ space_id: 7, space: 'Field team', space_kind: 'shared', board_id: 12, board: 'Maintenance' }],
    ...over,
  })

test('publishing shows what the draft does first, asks to acknowledge the schema changes the server names, then publishes with them', async () => {
  const changes = [{ change: 'field_removed', field: 'notes' }]
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces: [crewSpace(4, 'Crew')] } },
    'GET /api/workspaces/1/spaces/4/apps?archived=true': {
      body: { apps: [app({ space_id: 4, unpublished_changes: true, version: 5 })] },
    },
    'GET /api/workspaces/1/spaces/4/apps/1': { body: withEffects() },
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
  await choose(crew, 'Expenses', 'Publish…')
  const dialog = await screen.findByRole('dialog', { name: 'Publish Expenses' })

  await within(dialog).findByText('Creates a card on Maintenance (Field team).')
  expect(calls.some((c) => c.method === 'POST')).toBe(false)
  fireEvent.click(within(dialog).getByRole('button', { name: 'Review and publish…' }))
  expect(calls.some((c) => c.method === 'POST')).toBe(false)
  fireEvent.click(within(dialog).getByRole('button', { name: 'Publish for Crew' }))

  const panel = await screen.findByRole('dialog', { name: 'This publish may change how your app treats its data' })
  expect(within(panel).getByText(/“notes” is removed/)).toBeTruthy()
  const go = within(panel).getByRole('button', { name: 'Publish with these changes' }) as HTMLButtonElement
  expect(go.disabled).toBe(true)
  fireEvent.click(within(panel).getByRole('checkbox', { name: 'I understand these changes' }))
  expect(go.disabled).toBe(false)
  fireEvent.click(go)

  await waitFor(() =>
    expect(screen.queryByRole('dialog', { name: 'This publish may change how your app treats its data' })).toBeNull(),
  )
  await within(dialog).findByText(/Published as release 2/)
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
  await choose(crew, 'Expenses', 'History')
  const dialog = await screen.findByRole('dialog', { name: 'Release history of Expenses' })
  await within(dialog).findByText('Release 3')
  expect(within(crew).getByText('Live')).toBeTruthy()
  expect(within(crew).getAllByText(/Published by Ann/)).toHaveLength(2)
  expect(within(crew).getByText(/^Published Nov/)).toBeTruthy()
  expect(within(crew).getAllByRole('button', { name: /^Roll back to release/ })).toHaveLength(2)

  fireEvent.click(within(crew).getByRole('button', { name: 'Roll back to release 2' }))
  let ask = await screen.findByRole('alertdialog', { name: 'Roll back to release 2' })
  fireEvent.click(within(ask).getByRole('button', { name: 'Cancel' }))
  await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
  expect(calls.some((c) => c.method === 'POST')).toBe(false)

  fireEvent.click(within(crew).getByRole('button', { name: 'Roll back to release 2' }))
  ask = await screen.findByRole('alertdialog', { name: 'Roll back to release 2' })
  fireEvent.click(within(ask).getByRole('button', { name: 'Roll back' }))
  expect((await within(crew).findByRole('alert')).textContent).toBe(
    'The app’s data doesn’t fit that release, so it can’t go back to it. Fields that don’t fit: amount.',
  )
  fireEvent.click(within(crew).getByRole('button', { name: 'Roll back to release 1' }))
  ask = await screen.findByRole('alertdialog', { name: 'Roll back to release 1' })
  fireEvent.click(within(ask).getByRole('button', { name: 'Roll back' }))
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
  await choose(crew, 'Expenses', 'History')
  await screen.findByRole('dialog', { name: 'Release history of Expenses' })
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
  await waitFor(() => expect(shown(screen.getAllByText('Coffee'))).toHaveLength(1))
  fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
  await screen.findByText('Preview: nothing was saved')
  await screen.findByText('Fix Coffee')
  const sent = calls.filter((c) => c.method === 'POST')
  expect(sent.map((c) => c.path)).toEqual(Array(2).fill('/api/workspaces/1/spaces/2/apps/1/preview'))
  expect(sent[0].body).toEqual({})
  expect(sent[1].body).toMatchObject({ rows: [{ id: 1, revision: 1, data: {} }], action: { name: 'delete_expense' } })
  expect(calls.some((c) => c.path.endsWith('/actions'))).toBe(false)

  fireEvent.click(screen.getByRole('button', { name: 'Back to the live release' }))
  await screen.findByText('Live · release 4')
})

test('someone who may only use an app launches the live release and is offered no draft', async () => {
  window.history.replaceState(null, '', '/w/1/apps/2-1')
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
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

test('a refused rename is said in its dialog, which keeps what was typed and closes only once it is done', async () => {
  let refuse = true
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/spaces/2/apps?archived=true': () => ({
      body: { apps: [app(refuse ? {} : { name: 'Spending', version: 2 })] },
    }),
    'GET /api/workspaces/1/spaces/3/apps?archived=true': { body: { apps: [] } },
    'PATCH /api/workspaces/1/spaces/2/apps/1': () => {
      if (refuse) return { status: 409, body: { error: 'x' } }
      return { body: app({ name: 'Spending', version: 2 }) }
    },
  })
  render(<Routed />)
  const mine = await screen.findByRole('region', { name: 'Private' })
  await choose(mine, 'Expenses', 'Rename')
  const dialog = await screen.findByRole('dialog', { name: 'Rename Expenses' })
  fireEvent.change(within(dialog).getByLabelText('Name'), { target: { value: 'Spending' } })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }))

  expect((await within(dialog).findByRole('alert')).textContent).toBe(
    'Someone changed Expenses since the list was loaded. It is shown as it is now: try again.',
  )
  expect(screen.getByRole('dialog', { name: 'Rename Expenses' })).toBe(dialog)
  expect((within(dialog).getByLabelText('Name') as HTMLInputElement).value).toBe('Spending')
  expect(
    within(mine)
      .getAllByRole('alert')
      .every((a) => dialog.contains(a)),
  ).toBe(true)

  refuse = false
  fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  await within(mine).findByText('Spending')
  expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(2)
})

test('a rename being saved is not closed by Escape', async () => {
  let release: () => void = () => undefined
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/spaces/2/apps?archived=true': { body: { apps: [app({})] } },
    'GET /api/workspaces/1/spaces/3/apps?archived=true': { body: { apps: [] } },
    'PATCH /api/workspaces/1/spaces/2/apps/1': () =>
      new Promise((resolve) => {
        release = () => resolve({ body: app({ name: 'Spending', version: 2 }) })
      }),
  })
  render(<Routed />)
  const mine = await screen.findByRole('region', { name: 'Private' })
  await choose(mine, 'Expenses', 'Rename')
  const dialog = await screen.findByRole('dialog', { name: 'Rename Expenses' })
  fireEvent.change(within(dialog).getByLabelText('Name'), { target: { value: 'Spending' } })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(within(dialog).getByRole('button', { name: /Save/ }).getAttribute('aria-busy')).toBe('true'),
  )
  fireEvent(dialog, new Event('cancel', { cancelable: true }))
  expect(screen.getByRole('dialog', { name: 'Rename Expenses' })).toBe(dialog)
  release()
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
})

test('a refused move is said in its dialog; a move that is done leaves the focus on the space it left', async () => {
  const withTeam = {
    spaces: [
      ...spaces.spaces,
      { id: 4, workspace_id: 1, kind: 'shared', name: 'Crew', archived_at: null, role: 'editor', access: 'write' },
    ],
  }
  let refuse = true
  let mine = [app({})]
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: withTeam },
    'GET /api/workspaces/1/spaces/2/apps?archived=true': () => ({ body: { apps: mine } }),
    'GET /api/workspaces/1/spaces/3/apps?archived=true': { body: { apps: [] } },
    'GET /api/workspaces/1/spaces/4/apps?archived=true': () => ({
      body: { apps: refuse ? [] : [app({ space_id: 4 })] },
    }),
    'POST /api/workspaces/1/spaces/2/apps/1/move': () => {
      if (refuse) return { status: 403, body: { error: 'x', code: 'forbidden' } }
      mine = []
      return { body: app({ space_id: 4, version: 2 }) }
    },
  })
  render(<Routed />)
  const private_ = await screen.findByRole('region', { name: 'Private' })
  await choose(private_, 'Expenses', 'Move to a space…')
  const dialog = await screen.findByRole('dialog', { name: 'Move Expenses to a space' })
  fireEvent.change(within(dialog).getByRole('combobox', { name: 'Move Expenses to' }), { target: { value: '4' } })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Move' }))
  await within(dialog).findByRole('alert')
  expect(screen.getByRole('dialog', { name: 'Move Expenses to a space' })).toBe(dialog)

  refuse = false
  const heading = within(private_).getByRole('heading', { name: 'Private' })
  const dialogsWhenFocused: number[] = []
  heading.addEventListener('focus', () => dialogsWhenFocused.push(document.querySelectorAll('dialog').length))
  fireEvent.click(within(dialog).getByRole('button', { name: 'Move' }))
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  await within(screen.getByRole('region', { name: 'Crew' })).findByText('Expenses')
  expect(document.activeElement).toBe(heading)
  expect(dialogsWhenFocused).toEqual([0])
})

test('Move is offered only to those who may design the app, and Duplicate only where a copy can go', async () => {
  const noDesign = { ...everything, design: false, roles: ['publisher'], restricted: true }
  fakeFetch({
    'GET /api/workspaces/1/spaces': {
      body: {
        spaces: [
          { id: 3, workspace_id: 1, kind: 'shared', name: 'Team', archived_at: null, role: 'viewer', access: 'read' },
          crewSpace(4, 'Crew'),
          crewSpace(5, 'Field'),
        ],
      },
    },
    'GET /api/workspaces/1/spaces/3/apps?archived=true': {
      body: { apps: [app({ id: 7, space_id: 3, name: 'Viewed' })] },
    },
    'GET /api/workspaces/1/spaces/4/apps?archived=true': { body: { apps: [app({ space_id: 4, access: noDesign })] } },
    'GET /api/workspaces/1/spaces/5/apps?archived=true': { body: { apps: [] } },
  })
  render(<Routed />)
  const crew = await screen.findByRole('region', { name: 'Crew' })
  const items = await menuItems(crew, 'Expenses')
  expect(items).not.toContain('Move to a space…')
  expect(items).not.toContain('Rename')
  expect(items).toContain('Duplicate as template')
  cleanup()

  fakeFetch({
    'GET /api/workspaces/1/spaces': {
      body: {
        spaces: [
          { id: 3, workspace_id: 1, kind: 'shared', name: 'Team', archived_at: null, role: 'viewer', access: 'read' },
        ],
      },
    },
    'GET /api/workspaces/1/spaces/3/apps?archived=true': {
      body: { apps: [app({ id: 7, space_id: 3, name: 'Viewed' })] },
    },
  })
  render(<Routed />)
  const team = await screen.findByRole('region', { name: 'Team' })
  await within(team).findByRole('link', { name: 'Open Viewed' })
  expect(within(team).queryByRole('button', { name: 'More actions for Viewed' })).toBeNull()
})

test('in the list’s publish dialog, the confirmation takes the focus, described by what it does', async () => {
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: { spaces: [crewSpace(4, 'Crew')] } },
    'GET /api/workspaces/1/spaces/4/apps?archived=true': {
      body: { apps: [app({ space_id: 4, unpublished_changes: true, version: 5 })] },
    },
    'GET /api/workspaces/1/spaces/4/apps/1': { body: withEffects() },
  })
  render(<Routed />)
  const crew = await screen.findByRole('region', { name: 'Crew' })
  await choose(crew, 'Expenses', 'Publish…')
  const dialog = await screen.findByRole('dialog', { name: 'Publish Expenses' })
  fireEvent.click(await within(dialog).findByRole('button', { name: 'Review and publish…' }))
  const confirm = within(dialog).getByRole('button', { name: 'Publish for Crew' })
  await waitFor(() => expect(document.activeElement).toBe(confirm))
  const described = document.getElementById(confirm.getAttribute('aria-describedby') ?? '')
  expect(described?.textContent).toMatch(/live for everyone in Crew who may use it/)

  fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
  await waitFor(() =>
    expect(document.activeElement).toBe(within(dialog).getByRole('button', { name: 'Review and publish…' })),
  )
})

test('an icon is picked from the list or typed, saved with the version the person saw, and shown in the row', async () => {
  let expenses = app({})
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/spaces/2/apps?archived=true': () => ({ body: { apps: [expenses] } }),
    'GET /api/workspaces/1/spaces/3/apps?archived=true': { body: { apps: [] } },
    'PATCH /api/workspaces/1/spaces/2/apps/1': (call) => {
      const body = call.body as { version: number; icon: string }
      if (body.icon === '🛑') return { status: 400, body: { error: 'bad', code: 'app_icon_invalid' } }
      expenses = app({ ...expenses, version: expenses.version + 1, icon: body.icon })
      return { body: expenses }
    },
  })
  render(<Routed />)
  const mine = await screen.findByRole('region', { name: 'Private' })
  await within(mine).findByText('Expenses')
  expect(within(mine).getByText('E')).toBeTruthy()

  await choose(mine, 'Expenses', 'Change icon')
  let dialog = await screen.findByRole('dialog', { name: 'Icon for Expenses' })
  const save = within(dialog).getByRole('button', { name: 'Save' }) as HTMLButtonElement
  expect(save.disabled).toBe(true)
  const group = within(dialog).getByRole('radiogroup', { name: 'Change icon' })
  expect((within(group).getByRole('radio', { name: 'No icon (use initials)' }) as HTMLInputElement).checked).toBe(true)
  fireEvent.click(within(group).getByRole('radio', { name: '🧾' }))
  expect((within(group).getByRole('radio', { name: '🧾' }) as HTMLInputElement).checked).toBe(true)
  expect(save.disabled).toBe(false)
  fireEvent.click(save)
  await within(mine).findByText('🧾')
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  expect(within(mine).queryByText('E')).toBeNull()
  expect(calls.filter((c) => c.method === 'PATCH').map((c) => c.body)).toEqual([{ version: 1, icon: '🧾' }])

  await choose(mine, 'Expenses', 'Change icon')
  dialog = await screen.findByRole('dialog', { name: 'Icon for Expenses' })
  expect((within(dialog).getByRole('radio', { name: '🧾' }) as HTMLInputElement).checked).toBe(true)
  const typed = within(dialog).getByLabelText('Or type or paste an emoji')
  fireEvent.change(typed, { target: { value: 'abc' } })
  expect(within(dialog).getByText('Use a single emoji.')).toBeTruthy()
  expect((within(dialog).getByRole('button', { name: 'Save' }) as HTMLButtonElement).disabled).toBe(true)
  fireEvent.change(typed, { target: { value: '1' } })
  expect((within(dialog).getByRole('button', { name: 'Save' }) as HTMLButtonElement).disabled).toBe(true)
  fireEvent.change(typed, { target: { value: '🧾🧾' } })
  expect((within(dialog).getByRole('button', { name: 'Save' }) as HTMLButtonElement).disabled).toBe(true)
  fireEvent.change(typed, { target: { value: ' 🦄 ' } })
  expect(within(dialog).getByText('Use a single emoji.')).toBeTruthy()
  expect((within(dialog).getByRole('button', { name: 'Save' }) as HTMLButtonElement).disabled).toBe(true)
  fireEvent.change(typed, { target: { value: '🧾\u200b' } })
  expect((within(dialog).getByRole('button', { name: 'Save' }) as HTMLButtonElement).disabled).toBe(true)
  fireEvent.change(typed, { target: { value: '🦄' } })
  expect(within(dialog).queryByText('Use a single emoji.')).toBeNull()
  fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }))
  await within(mine).findByText('🦄')
  expect(calls.filter((c) => c.method === 'PATCH').at(-1)?.body).toEqual({ version: 2, icon: '🦄' })

  await choose(mine, 'Expenses', 'Change icon')
  dialog = await screen.findByRole('dialog', { name: 'Icon for Expenses' })
  expect((within(dialog).getByLabelText('Or type or paste an emoji') as HTMLInputElement).value).toBe('🦄')
  fireEvent.click(within(dialog).getByRole('radio', { name: 'No icon (use initials)' }))
  fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }))
  await within(mine).findByText('E')
  expect(calls.filter((c) => c.method === 'PATCH').at(-1)?.body).toEqual({ version: 3, icon: '' })

  await choose(mine, 'Expenses', 'Change icon')
  dialog = await screen.findByRole('dialog', { name: 'Icon for Expenses' })
  fireEvent.change(within(dialog).getByLabelText('Or type or paste an emoji'), { target: { value: '🛑' } })
  fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(calls.filter((c) => c.method === 'PATCH')).toHaveLength(4))
  expect(screen.getByRole('dialog', { name: 'Icon for Expenses' })).toBeTruthy()
})

test('the icon is changed only by someone who may design the app in a space they may write', async () => {
  fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/spaces/2/apps?archived=true': {
      body: { apps: [app({ access: { ...everything, design: false } }), app({ id: 2, name: 'Mine', icon: '🧾' })] },
    },
    'GET /api/workspaces/1/spaces/3/apps?archived=true': {
      body: { apps: [app({ id: 7, space_id: 3, name: 'Team tracker', icon: '🚚' })] },
    },
  })
  render(<Routed />)
  const mine = await screen.findByRole('region', { name: 'Private' })
  const team = screen.getByRole('region', { name: 'Team' })
  await within(team).findByText('Team tracker')
  expect(within(team).getByText('🚚')).toBeTruthy()
  expect(await menuItems(team, 'Team tracker')).not.toContain('Change icon')
  await within(mine).findByText('Mine')
  expect(within(mine).getByText('🧾')).toBeTruthy()
  expect(await menuItems(mine, 'Expenses')).not.toContain('Change icon')
  expect(await menuItems(mine, 'Mine')).toContain('Change icon')
})

test('a pin in the sidebar shows the new icon as soon as the icon is saved, with no reload', async () => {
  let expenses = app({})
  const pin = (icon: string) => ({
    id: 11,
    source: 'personal',
    kind: 'pin',
    space_id: 2,
    app_id: 1,
    app_name: 'Expenses',
    app_icon: icon,
    label: 'Expenses',
    state: 'ready',
  })
  const calls = fakeFetch({
    'GET /api/workspaces/1/spaces': { body: spaces },
    'GET /api/workspaces/1/spaces/2/apps?archived=true': () => ({ body: { apps: [expenses] } }),
    'GET /api/workspaces/1/spaces/3/apps?archived=true': { body: { apps: [] } },
    'GET /api/workspaces/1/home': () => ({
      body: { widgets: [], pins: [pin(expenses.icon)], hidden: [], limits: { widgets: 24, pins: 20 } },
    }),
    'PATCH /api/workspaces/1/spaces/2/apps/1': (call) => {
      expenses = app({ ...expenses, version: expenses.version + 1, icon: (call.body as { icon: string }).icon })
      return { body: expenses }
    },
  })
  render(
    <>
      <PinnedNav workspace={1} path="/w/1/apps" />
      <Routed />
    </>,
  )
  const pinned = await screen.findByRole('region', { name: 'Pinned apps' })
  const link = within(pinned).getByRole('link', { name: 'Expenses' })
  expect(within(link).getByText('EX')).toBeTruthy()
  const mine = await screen.findByRole('region', { name: 'Private' })
  await choose(mine, 'Expenses', 'Change icon')
  const dialog = await screen.findByRole('dialog', { name: 'Icon for Expenses' })
  fireEvent.click(within(dialog).getByRole('radio', { name: '🧾' }))
  fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }))
  await within(mine).findByText('🧾')
  await waitFor(() =>
    expect(within(within(pinned).getByRole('link', { name: 'Expenses' })).getByText('🧾')).toBeTruthy(),
  )
  expect(calls.filter((c) => c.path === '/api/workspaces/1/home')).toHaveLength(2)
})
