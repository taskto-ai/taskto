// @vitest-environment jsdom

import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { setPreferences } from '../i18n'
import { parseRoute, usePath } from '../router'
import { fakeFetch } from '../test/fakes'
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
// A space they only read: its boards are never offered.
const readOnly = {
  id: 4,
  workspace_id: 1,
  kind: 'shared',
  name: 'Reading room',
  archived_at: null,
  role: 'viewer',
  access: 'read',
}
const spaces = { spaces: [privateSpace, teamSpace, readOnly] }

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
const needs = { definition_version: 2, requires: ['component.Column', 'component.Text'] }
const draftDefinition = {
  ...needs,
  collection: { fields: [{ name: 'description', type: 'string' }] },
  actions: { add_expense: { kind: 'insert' } },
}
const app = (over: Record<string, unknown> = {}) => ({
  id: 9,
  space_id: 2,
  name: 'Expense tracker',
  archived_at: null,
  version: 1,
  rows: 0,
  access: everything,
  live: null,
  unpublished_changes: true,
  draft: draftDefinition,
  ...over,
})

const templates = [
  {
    id: 'equipment-checkout',
    version: 1,
    name: 'Equipment checkout',
    description: 'Scan an item’s tag.',
    recommended_space: 'shared',
    needs_board: false,
    captures: ['barcode'],
    roles: { installer: ['builder', 'publisher', 'data_manager'], members: ['user'] },
    widgets: ['Equipment'],
  },
  {
    id: 'expense-tracker',
    version: 1,
    name: 'Expense tracker',
    description: 'Record what you spend.',
    recommended_space: 'private',
    needs_board: false,
    captures: ['attachment'],
    roles: { installer: ['builder', 'publisher', 'user'], members: ['user'] },
    widgets: ['Expenses'],
  },
  {
    id: 'inspection',
    version: 1,
    name: 'Inspection',
    description: 'Walk a site.',
    recommended_space: 'shared',
    needs_board: true,
    captures: ['location', 'signature'],
    widgets: ['Inspections'],
  },
]

const boards = (space: number, names: string[]) => ({
  body: {
    boards: names.map((name, i) => ({
      id: space * 10 + i,
      space_id: space,
      kind: i === 0 && space === 2 ? 'personal' : 'team',
      name,
      archived_at: null,
      version: 1,
    })),
  },
})

function Routed() {
  const route = parseRoute(usePath())
  return route ? <AppsPage workspace={route.workspace} open={route.item} /> : null
}

beforeEach(() => window.history.replaceState(null, '', '/w/1/apps'))
afterEach(async () => {
  cleanup()
  vi.unstubAllGlobals()
  await setPreferences({ user: 'en', workspace: null })
})

const base = {
  'GET /api/workspaces/1/spaces': { body: spaces },
  'GET /api/workspaces/1/app-templates': { body: { templates } },
}

test('the Apps page starts from a template, listing each with what it needs', async () => {
  fakeFetch({ ...base, 'GET /api/workspaces/1/spaces/2/apps?archived=true': { body: { apps: [] } } })
  render(<Routed />)
  fireEvent.click(await screen.findByRole('link', { name: 'Start from a template' }))
  await screen.findByRole('heading', { name: 'Start from a template', level: 1 })
  const cards = await screen.findAllByRole('heading', { level: 2 })
  expect(cards.map((c) => c.textContent)).toEqual(['Equipment checkout', 'Expense tracker', 'Inspection'])
  const inspection = screen.getByRole('button', { name: 'Use the template Inspection' }).closest('li') as HTMLElement
  expect(within(inspection).getByText(/Needs a board of yours/)).toBeTruthy()
  expect(within(inspection).getByText('Uses your phone for: your position, a signature pad.')).toBeTruthy()
  const expense = screen.getByRole('button', { name: 'Use the template Expense tracker' }).closest('li') as HTMLElement
  expect(within(expense).getByText('Uses your phone for: the camera.')).toBeTruthy()
  expect(within(expense).queryByText(/Needs a board/)).toBeNull()
})

test('installs into the private space by default and opens the draft with its audience and publish', async () => {
  let installed = false
  const calls = fakeFetch({
    ...base,
    'POST /api/workspaces/1/spaces/2/app-templates/expense-tracker/install': () => {
      installed = true
      return { status: 201, body: app() }
    },
    'GET /api/workspaces/1/spaces/2/apps/9': () => ({ body: installed ? app() : app({ id: 0 }) }),
    'POST /api/workspaces/1/spaces/2/apps/9/preview': { body: { messages: surface, rows: [] } },
    'POST /api/workspaces/1/spaces/2/apps/9/publish': {
      body: app({
        live: { ...needs, number: 1, published_at: 'x', published_by: null, live: true },
        unpublished_changes: false,
      }),
    },
  })
  window.history.replaceState(null, '', '/w/1/apps/templates')
  render(<Routed />)
  fireEvent.click(await screen.findByRole('button', { name: 'Use the template Expense tracker' }))
  const form = (await screen.findByRole('button', { name: 'Install as a draft' })).closest('form') as HTMLFormElement
  // Private is the default; a shared space is a choice, and a template without a board asks for none.
  expect((within(form).getByLabelText('In my private space') as HTMLInputElement).checked).toBe(true)
  expect(within(form).queryByLabelText('Board for the cards it makes')).toBeNull()
  expect((within(form).getByLabelText('Name') as HTMLInputElement).value).toBe('Expense tracker')
  fireEvent.click(within(form).getByRole('button', { name: 'Install as a draft' }))

  await waitFor(() => expect(window.location.pathname).toBe('/w/1/apps/installed-2-9'))
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ name: 'Expense tracker' })
  await screen.findByRole('heading', { name: 'Expense tracker', level: 1 })
  expect(screen.getByText(/This is a draft: nothing is live yet/)).toBeTruthy()
  const panel = await screen.findByRole('region', { name: 'Publish' })
  expect(within(panel).getByText('Only you: it is in your private space.')).toBeTruthy()
  fireEvent.click(within(panel).getByRole('button', { name: 'Publish' }))
  await within(panel).findByText(/Published as release 1/)
})

test('once published, the install page shows the live app and no longer offers to publish', async () => {
  let published = false
  const live = { ...needs, number: 1, published_at: 'x', published_by: null, live: true }
  const calls = fakeFetch({
    ...base,
    'GET /api/workspaces/1/spaces/2/apps/9': () => ({
      body: published ? app({ live, unpublished_changes: false }) : app(),
    }),
    'POST /api/workspaces/1/spaces/2/apps/9/preview': { body: { messages: surface, rows: [] } },
    'GET /api/workspaces/1/spaces/2/apps/9/surface?release=1': { body: surface },
    'POST /api/workspaces/1/spaces/2/apps/9/publish': () => {
      published = true
      return { body: app({ live, unpublished_changes: false }) }
    },
  })
  window.history.replaceState(null, '', '/w/1/apps/installed-2-9')
  render(<Routed />)
  // A draft nobody has published is previewed on sample data, with the way to publish it.
  await screen.findByText('Preview · sample data')
  const panel = await screen.findByRole('region', { name: 'Publish' })
  fireEvent.click(within(panel).getByRole('button', { name: 'Publish' }))
  await within(panel).findByText(/Published as release 1/)

  // Nothing is unpublished now: the live app is what the page shows, and says so.
  await screen.findByText('Live · release 1')
  expect(screen.queryByText('Preview · sample data')).toBeNull()
  expect(screen.getByText(/Nothing is unpublished: the draft is the live release/)).toBeTruthy()
  expect(screen.queryByRole('button', { name: 'Preview the draft' })).toBeNull()
  expect(within(panel).queryByRole('button', { name: /ublish/ })).toBeNull()
  expect(calls.some((c) => c.path === '/api/workspaces/1/spaces/2/apps/9/surface?release=1')).toBe(true)
  // And the way to the app where people use it.
  expect(screen.getByRole('link', { name: 'Open Expense tracker in Apps' }).getAttribute('href')).toBe('/w/1/apps/2-9')
})

test('a template that makes cards needs a board the person may write: only those are offered', async () => {
  let refuse = true
  const calls = fakeFetch({
    ...base,
    'GET /api/workspaces/1/spaces/2/boards': boards(2, ['Personal']),
    'GET /api/workspaces/1/spaces/3/boards': boards(3, ['Repairs']),
    'GET /api/workspaces/1/spaces/4/boards': boards(4, ['Secret plans']),
    'POST /api/workspaces/1/spaces/2/app-templates/inspection/install': (call) => {
      if (refuse) {
        refuse = false
        return { status: 400, body: { code: 'app_effect_target', error: 'x', params: { space_id: 3 } } }
      }
      expect((call.body as { board: unknown }).board).toEqual({ space_id: 3, board_id: 30 })
      return { status: 201, body: app({ id: 12, name: 'Inspection' }) }
    },
    'GET /api/workspaces/1/spaces/2/apps/12': { body: app({ id: 12, name: 'Inspection' }) },
    'POST /api/workspaces/1/spaces/2/apps/12/preview': { body: { messages: surface, rows: [] } },
  })
  window.history.replaceState(null, '', '/w/1/apps/templates')
  render(<Routed />)
  fireEvent.click(await screen.findByRole('button', { name: 'Use the template Inspection' }))
  const install = (await screen.findByRole('button', { name: 'Install as a draft' })) as HTMLButtonElement
  const form = install.closest('form') as HTMLFormElement
  const board = (await within(form).findByLabelText('Board for the cards it makes')) as HTMLSelectElement
  await waitFor(() => expect(board.options.length).toBe(3))
  expect([...board.options].map((o) => o.textContent)).toEqual([
    'Choose a board…',
    'Personal · Private',
    'Repairs · Team',
  ])
  expect(calls.some((c) => c.path.includes('/spaces/4/boards'))).toBe(false)
  // Without a board it can't be installed.
  expect(install.disabled).toBe(true)
  fireEvent.change(board, { target: { value: '3:30' } })
  expect(install.disabled).toBe(false)
  fireEvent.click(install)
  // The server's refusal is said in words, and nothing moves on.
  await within(form).findByText(/targets a space or board you can’t write to/)
  expect(window.location.pathname).toBe('/w/1/apps/templates')
  fireEvent.click(install)
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/apps/installed-2-12'))
})

test('a shared space is chosen explicitly, and says which roles the install gives', async () => {
  const calls = fakeFetch({
    ...base,
    'POST /api/workspaces/1/spaces/3/app-templates/equipment-checkout/install': {
      status: 201,
      body: app({ id: 5, space_id: 3 }),
    },
    'GET /api/workspaces/1/spaces/3/apps/5': {
      body: app({
        id: 5,
        space_id: 3,
        access: { ...everything, restricted: true, roles: ['builder', 'publisher', 'data_manager'] },
      }),
    },
    'GET /api/workspaces/1/spaces/3/apps/5/roles': {
      body: {
        grants: [
          { user_id: 7, roles: ['builder', 'publisher', 'data_manager'] },
          { user_id: null, roles: ['user'] },
        ],
      },
    },
    'GET /api/workspaces/1/spaces/3/members': {
      body: { members: [{ id: 7, email: 'ann@example.com', name: 'Ann', role: 'editor', added_at: 'x' }] },
    },
    'POST /api/workspaces/1/spaces/3/apps/5/preview': { body: { messages: surface, rows: [] } },
  })
  window.history.replaceState(null, '', '/w/1/apps/templates')
  render(<Routed />)
  fireEvent.click(await screen.findByRole('button', { name: 'Use the template Equipment checkout' }))
  const form = (await screen.findByRole('button', { name: 'Install as a draft' })).closest('form') as HTMLFormElement
  // Private is still the default, with a note that this one is made for a team.
  expect((within(form).getByLabelText('In my private space') as HTMLInputElement).checked).toBe(true)
  expect(within(form).getByText(/made for a team in a shared space/)).toBeTruthy()
  fireEvent.click(within(form).getByLabelText('In a shared space'))
  fireEvent.change(within(form).getByLabelText('Shared space'), { target: { value: '3' } })
  expect(
    within(form).getByText(
      'In a shared space, you get these roles: Builder, Publisher, Data manager. Everyone else in the space gets: User.',
    ),
  ).toBeTruthy()
  fireEvent.click(within(form).getByRole('button', { name: 'Install as a draft' }))
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/apps/installed-3-5'))
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ name: 'Equipment checkout' })
  // The audience panel names who holds which role, before anything is published.
  const panel = await screen.findByRole('region', { name: 'Publish' })
  await within(panel).findByText('Ann: Builder, Publisher, Data manager')
  expect(within(panel).getByText('Everyone else: User')).toBeTruthy()
})

test('a draft with no board chosen asks for one and can’t be published until it has one', async () => {
  let needsBoard = true
  const calls = fakeFetch({
    ...base,
    'GET /api/workspaces/1/spaces/2/boards': boards(2, ['Personal']),
    'GET /api/workspaces/1/spaces/3/boards': boards(3, ['Repairs']),
    'GET /api/workspaces/1/spaces/4/boards': boards(4, ['Secret plans']),
    'GET /api/workspaces/1/spaces/2/apps/9': () => ({
      body: app({ needs_board: needsBoard, version: needsBoard ? 1 : 2 }),
    }),
    'POST /api/workspaces/1/spaces/2/apps/9/preview': { body: { messages: surface, rows: [] } },
    'POST /api/workspaces/1/spaces/2/apps/9/effect-board': () => {
      needsBoard = false
      return { body: app({ version: 2 }) }
    },
  })
  window.history.replaceState(null, '', '/w/1/apps/installed-2-9')
  render(<Routed />)
  const chooser = await screen.findByRole('form', { name: 'Board for the cards it makes' })
  expect(within(chooser).getByText(/none is chosen yet/)).toBeTruthy()
  const panel = await screen.findByRole('region', { name: 'Publish' })
  expect(within(panel).getByText(/can’t be published until then/)).toBeTruthy()
  expect(within(panel).queryByRole('button', { name: /ublish/ })).toBeNull()
  const select = (await within(chooser).findByLabelText('Board for the cards it makes')) as HTMLSelectElement
  await waitFor(() => expect(select.options.length).toBe(3))
  const use = within(chooser).getByRole('button', { name: 'Use this board' }) as HTMLButtonElement
  expect(use.disabled).toBe(true)
  fireEvent.change(select, { target: { value: '3:30' } })
  fireEvent.click(use)
  await waitFor(() => expect(screen.queryByRole('form', { name: 'Board for the cards it makes' })).toBeNull())
  expect(calls.find((c) => c.path.endsWith('/effect-board'))?.body).toEqual({ version: 1, space_id: 3, board_id: 30 })
  await within(await screen.findByRole('region', { name: 'Publish' })).findByRole('button', { name: 'Publish' })
})

test('duplicating an app copies the definition into a space the person picks, private by default', async () => {
  const source = app({
    id: 1,
    name: 'Expenses',
    live: { ...needs, number: 1, published_at: 'x', published_by: null, live: true },
    unpublished_changes: false,
  })
  const calls = fakeFetch({
    ...base,
    'GET /api/workspaces/1/spaces/2/apps?archived=true': { body: { apps: [] } },
    'GET /api/workspaces/1/spaces/3/apps?archived=true': { body: { apps: [{ ...source, space_id: 3 }] } },
    'GET /api/workspaces/1/spaces/4/apps?archived=true': { body: { apps: [] } },
    'POST /api/workspaces/1/spaces/3/apps/1/duplicate': {
      status: 201,
      body: app({ id: 20, space_id: 2, name: 'Copy of Expenses' }),
    },
    'GET /api/workspaces/1/spaces/2/apps/20': { body: app({ id: 20, name: 'Copy of Expenses', needs_board: true }) },
    'POST /api/workspaces/1/spaces/2/apps/20/preview': { body: { messages: surface, rows: [] } },
  })
  render(<Routed />)
  const team = await screen.findByRole('region', { name: 'Team' })
  fireEvent.click(await within(team).findByRole('button', { name: 'Duplicate Expenses as a template' }))
  const form = await within(team).findByRole('form', { name: 'Duplicate Expenses' })
  expect(within(form).getByText(/no records, photos, history or roles come with it/)).toBeTruthy()
  expect((within(form).getByLabelText('Name') as HTMLInputElement).value).toBe('Copy of Expenses')
  // The read-only space isn't a place to put it.
  const to = within(form).getByLabelText('Put the copy in') as HTMLSelectElement
  expect([...to.options].map((o) => o.textContent)).toEqual(['Team', 'Private'])
  expect(to.value).toBe('2')
  fireEvent.click(within(form).getByRole('button', { name: 'Make the copy' }))
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/apps/installed-2-20'))
  expect(calls.find((c) => c.path.endsWith('/duplicate'))?.body).toEqual({ name: 'Copy of Expenses', space_id: 2 })
  // The copy had boards the copier can't write: it asks for one before it can be published.
  await screen.findByRole('form', { name: 'Board for the cards it makes' })
})

test('the templates are in Portuguese too', async () => {
  await setPreferences({ user: 'pt-BR', workspace: null })
  fakeFetch({ ...base, 'GET /api/workspaces/1/spaces/2/apps?archived=true': { body: { apps: [] } } })
  window.history.replaceState(null, '', '/w/1/apps/templates')
  render(<Routed />)
  expect(await screen.findByRole('heading', { name: 'Controle de despesas', level: 2 })).toBeTruthy()
  expect(screen.getByRole('heading', { name: 'Retirada de equipamentos', level: 2 })).toBeTruthy()
  expect(screen.getByRole('heading', { name: 'Inspeção', level: 2 })).toBeTruthy()
})

test('the template screens use logical properties and fit a phone', () => {
  const css = readFileSync(join(process.cwd(), 'src/apps/apps.css'), 'utf8')
  const block = css.slice(css.indexOf('/* Templates:'))
  // One column at 360px: a card is never wider than the screen, whatever the minimum says.
  expect(block).toContain('minmax(min(100%, 18rem), 1fr)')
  expect(block).not.toMatch(
    /(margin|padding)-(left|right)|\b(left|right):|text-align: (left|right)|border-(left|right)|\bwidth:|max-width:/,
  )
})
