// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { parseRoute, usePath } from '../router'
import { fakeFetch } from '../test/fakes'
import { AppsPage } from './AppsPage'

const spaces = {
  spaces: [
    { id: 2, workspace_id: 1, kind: 'private', name: 'Private', archived_at: null, role: 'owner', access: 'write' },
    { id: 3, workspace_id: 1, kind: 'shared', name: 'Team', archived_at: null, role: 'viewer', access: 'read' },
  ],
}
const app = (over: Record<string, unknown>) => ({
  id: 1,
  space_id: 2,
  name: 'Expenses',
  archived_at: null,
  version: 1,
  rows: 1,
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
    'GET /api/workspaces/1/spaces/2/apps/1/surface': { body: surface },
  })
  render(<Routed />)
  fireEvent.click(await screen.findByRole('link', { name: 'Launch Expenses' }))
  expect(window.location.pathname).toBe('/w/1/apps/2-1')
  await screen.findByRole('heading', { name: 'Expenses' })
  await screen.findByText('Coffee')
  expect(screen.getByRole('region', { name: 'Expenses' })).toBeTruthy()
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
    'GET /api/workspaces/1/spaces/2/apps/1/surface': { body: surface },
  })
  render(<Routed />)
  await screen.findByText(/This app is offline/)
  expect(calls.some((c) => c.path.endsWith('/surface'))).toBe(false)
  fireEvent.click(await screen.findByRole('button', { name: 'Restore' }))
  await screen.findByText('Coffee')
  await waitFor(() => expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ version: 2, archived: false }))
})
