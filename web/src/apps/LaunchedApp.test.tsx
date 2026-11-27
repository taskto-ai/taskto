// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import surface from '../../../internal/apps/testdata/expenses.surface.json'
import { fakeFetch } from '../test/fakes'
import { LaunchedApp } from './LaunchedApp'
import { forgetOperations } from './operations'

afterEach(() => {
  cleanup()
  forgetOperations()
  vi.unstubAllGlobals()
})

const base = '/api/workspaces/1/spaces/2/apps/1'
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
const definition = { collection: { fields: [{ name: 'description', type: 'string' }] }, actions: {} }
const app = (over: Record<string, unknown> = {}) => ({
  id: 1,
  space_id: 2,
  name: 'Expenses',
  archived_at: null,
  version: 1,
  rows: 1,
  access: everything,
  live: {
    number: 1,
    definition_version: 1,
    requires: ['component.Column', 'component.Text'],
    definition,
    published_at: '2026-11-18T10:00:00.000Z',
    published_by: { id: 1, name: 'Ann', agent: false },
    live: true,
  },
  unpublished_changes: false,
  draft: { definition_version: 1, requires: [] },
  ...over,
})

function serve(over: Record<string, unknown> = {}) {
  return fakeFetch({
    [`GET ${base}`]: { body: app(over) },
    [`GET ${base}/surface?release=1`]: { body: surface },
    [`GET ${base}/effects`]: { body: { effects: [], active: 0, failed: 0 } },
    [`GET ${base}/records/query`]: {
      body: { records: [], next_cursor: null, total: 0, aggregates: [] },
    },
  })
}

test("the app's name heads the page, with the way back, to its records and to editing it", async () => {
  serve()
  render(<LaunchedApp workspace={1} space={2} app={1} writable />)
  const heading = await screen.findByRole('heading', { level: 1, name: 'Expenses' })
  const header = heading.closest('header') as HTMLElement
  expect(within(header).getByRole('link', { name: 'All apps' }).getAttribute('href')).toBe('/w/1/apps')
  expect(within(header).getByRole('link', { name: 'Edit Expenses with AI' })).toBeTruthy()
  expect(within(header).getByRole('button', { name: 'Records and history' })).toBeTruthy()
})

test("nothing the frame draws is a primary button: the app's own is the page's one", async () => {
  serve()
  const { container } = render(<LaunchedApp workspace={1} space={2} app={1} writable />)
  await screen.findByText('Coffee')
  expect(container.querySelectorAll('.ui-button').length).toBeGreaterThan(0)
  expect(container.querySelectorAll('.ui-button-primary')).toHaveLength(0)
})

test('opening the records shows them under the app and takes the keyboard there; closing gives it back', async () => {
  serve()
  render(<LaunchedApp workspace={1} space={2} app={1} writable />)
  const toggle = await screen.findByRole('button', { name: 'Records and history' })
  expect(toggle.getAttribute('aria-expanded')).toBe('false')
  fireEvent.click(toggle)
  const panel = await screen.findByRole('region', { name: 'Records and history' })
  expect(toggle.getAttribute('aria-expanded')).toBe('true')
  expect(toggle.getAttribute('aria-controls')).toBe(panel.id)
  await waitFor(() => expect(document.activeElement).toBe(within(panel).getByRole('heading', { level: 2 })))
  await within(panel).findByText('No records yet.')

  fireEvent.click(within(panel).getByRole('button', { name: 'Hide records and history' }))
  expect(screen.queryByRole('region', { name: 'Records and history' })).toBeNull()
  expect(toggle.getAttribute('aria-expanded')).toBe('false')
  expect(document.activeElement).toBe(toggle)
})

test('a reader without records sees no way to them, and a viewer no way to edit', async () => {
  serve({ access: { ...everything, design: false, read_records: 'none' } })
  render(<LaunchedApp workspace={1} space={2} app={1} writable={false} />)
  await screen.findByRole('heading', { level: 1, name: 'Expenses' })
  expect(screen.queryByRole('button', { name: 'Records and history' })).toBeNull()
  expect(screen.queryByRole('link', { name: /with AI/ })).toBeNull()
  expect(screen.getByText(/You can view this app/)).toBeTruthy()
})

test('an offline app says so, and its restore is the one primary action', async () => {
  serve({ archived_at: '2026-11-18T00:00:00.000Z' })
  render(<LaunchedApp workspace={1} space={2} app={1} writable />)
  const restore = await screen.findByRole('button', { name: 'Restore' })
  expect(restore.className).toContain('ui-button-primary')
  expect(screen.getByText(/This app is offline/)).toBeTruthy()
  expect(screen.queryByRole('button', { name: 'Records and history' })).toBeNull()
})

test('editing with the agent takes both a space to write in and the right to design the app', async () => {
  serve()
  const { unmount } = render(<LaunchedApp workspace={1} space={2} app={1} writable={false} />)
  await screen.findByRole('heading', { level: 1, name: 'Expenses' })
  expect(screen.queryByRole('link', { name: /with AI/ })).toBeNull()
  unmount()
  cleanup()

  serve({ access: { ...everything, design: false } })
  render(<LaunchedApp workspace={1} space={2} app={1} writable />)
  await screen.findByRole('heading', { level: 1, name: 'Expenses' })
  expect(screen.queryByRole('link', { name: /with AI/ })).toBeNull()
  expect(screen.getByRole('button', { name: 'Records and history' })).toBeTruthy()
})

test('only someone who may publish the app restores it; nobody sees an offline app or its offline use', async () => {
  const calls = serve({ archived_at: '2026-11-18T00:00:00.000Z', access: { ...everything, publish: false } })
  render(<LaunchedApp workspace={1} space={2} app={1} writable />)
  await screen.findByText(/This app is offline/)
  expect(screen.queryByRole('button', { name: 'Restore' })).toBeNull()
  expect(screen.queryByRole('region', { name: 'Expenses' })).toBeNull()
  expect(screen.queryByRole('region', { name: 'Offline use' })).toBeNull()
  expect(calls.some((c) => c.path.includes('/surface'))).toBe(false)
})

test('a person who sees only their own records is told so under the name', async () => {
  serve({ access: { ...everything, read_records: 'own' } })
  render(<LaunchedApp workspace={1} space={2} app={1} writable />)
  await screen.findByRole('heading', { level: 1, name: 'Expenses' })
  expect(screen.getByText('You see only the records you added.')).toBeTruthy()
})

test('opening the records scrolls the page to the panel’s top, leaving the heading’s focus ring its room', async () => {
  serve()
  const { container } = render(
    <div data-testid="page" style={{ overflowY: 'auto' }}>
      <LaunchedApp workspace={1} space={2} app={1} writable />
    </div>,
  )
  const page = container.firstElementChild as HTMLElement
  Object.defineProperty(page, 'scrollHeight', { value: 2000 })
  Object.defineProperty(page, 'clientHeight', { value: 800 })
  const toggle = await screen.findByRole('button', { name: 'Records and history' })
  const panelId = toggle.getAttribute('aria-controls') as string
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
    const top = this.id === panelId ? 700 : this.id === `${panelId}-title` ? 716 : 0
    return { top, left: 0, right: 0, bottom: top, width: 0, height: 0, x: 0, y: top, toJSON: () => ({}) } as DOMRect
  })
  fireEvent.click(toggle)
  await screen.findByRole('region', { name: 'Records and history' })
  await waitFor(() => expect(page.scrollTop).toBe(700))
})

test('closing the records with an unsaved change in a record asks first, from the header and from the panel', async () => {
  const lunch = {
    id: 4,
    revision: 2,
    data: { description: 'Lunch' },
    created_at: '2026-11-01T10:00:00.000Z',
    created_by: null,
    updated_at: '2026-11-02T10:00:00.000Z',
    updated_by: null,
    deleted_at: null,
    deleted_by: null,
  }
  const editable = app({
    live: { ...app().live, definition: { ...definition, actions: { edit: { kind: 'update' } } } },
  })
  fakeFetch({
    [`GET ${base}`]: { body: editable },
    [`GET ${base}/surface?release=1`]: { body: surface },
    [`GET ${base}/effects`]: { body: { effects: [], active: 0, failed: 0 } },
    [`GET ${base}/records/query`]: { body: { records: [lunch], next_cursor: null, total: 1, aggregates: [] } },
    [`GET ${base}/records/4/history`]: { body: { record: lunch, history: [], truncated: false } },
  })
  render(<LaunchedApp workspace={1} space={2} app={1} writable />)
  const toggle = await screen.findByRole('button', { name: 'Records and history' })
  fireEvent.click(toggle)
  fireEvent.click(await screen.findByRole('button', { name: 'Open record 4' }))
  const drawer = await screen.findByRole('region', { name: 'Record 4' })
  const description = (await waitFor(() => within(drawer).getByLabelText(/^description/))) as HTMLInputElement
  fireEvent.change(description, { target: { value: 'Team lunch' } })
  const ask = () => screen.findByRole('alertdialog', { name: 'Discard your changes to this record?' })

  fireEvent.click(toggle)
  fireEvent.click(within(await ask()).getByRole('button', { name: 'Keep editing' }))
  await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
  expect(screen.getByRole('region', { name: 'Records and history' })).toBeTruthy()
  expect((within(drawer).getByLabelText(/^description/) as HTMLInputElement).value).toBe('Team lunch')

  const panel = screen.getByRole('region', { name: 'Records and history' })
  fireEvent.click(within(panel).getByRole('button', { name: 'Hide records and history' }))
  fireEvent.click(within(await ask()).getByRole('button', { name: 'Discard my changes' }))
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Records and history' })).toBeNull())
})

test('standalone, the app has no way back to the list and none to its builder, and fills the width', async () => {
  serve()
  const { container } = render(<LaunchedApp standalone workspace={1} space={2} app={1} writable />)
  const heading = await screen.findByRole('heading', { level: 1, name: 'Expenses' })
  const header = heading.closest('header') as HTMLElement
  expect(within(header).queryByRole('link', { name: 'All apps' })).toBeNull()
  expect(within(header).queryByRole('link', { name: /with AI/ })).toBeNull()
  expect(within(header).queryByRole('button', { name: 'Open in a new window' })).toBeNull()
  expect(within(header).queryByRole('link', { name: 'Open in a new tab' })).toBeNull()
  expect(within(header).getByRole('link', { name: 'Open in taskto' }).getAttribute('href')).toBe('/w/1/apps/2-1')
  expect(within(header).getByRole('button', { name: 'Records and history' })).toBeTruthy()
  expect(container.querySelector('.ui-page')?.getAttribute('data-width')).toBe('full')
})

test('standalone, the browser tab is named for the app, and the title is put back when it goes', async () => {
  serve()
  document.title = 'before'
  const { unmount } = render(<LaunchedApp standalone workspace={1} space={2} app={1} writable screen="home" />)
  await waitFor(() => expect(document.title).toBe('Expenses · taskto.ai'))
  expect(screen.getByRole('link', { name: 'Open in taskto' }).getAttribute('href')).toBe('/w/1/apps/2-1-home')
  unmount()
  expect(document.title).toBe('before')
})

test('in the shell the title is left alone', async () => {
  serve()
  document.title = 'before'
  render(<LaunchedApp workspace={1} space={2} app={1} writable />)
  await screen.findByRole('heading', { level: 1, name: 'Expenses' })
  expect(document.title).toBe('before')
})

test('the app opens in a window of its own, a popup named for the app, or in a new tab by its link', async () => {
  serve()
  const open = vi.fn()
  vi.stubGlobal('open', open)
  render(<LaunchedApp workspace={1} space={2} app={1} writable screen="home" />)
  const windowButton = await screen.findByRole('button', { name: 'Open in a new window' })
  fireEvent.click(windowButton)
  expect(open).toHaveBeenCalledTimes(1)
  expect(open).toHaveBeenCalledWith('/w/1/run/2-1-home', 'taskto-app-2-1', 'popup,width=480,height=800')
  const tab = screen.getByRole('link', { name: 'Open in a new tab' })
  expect(tab.getAttribute('href')).toBe('/w/1/run/2-1-home')
  expect(tab.getAttribute('target')).toBe('_blank')
  expect(tab.getAttribute('rel')).toContain('noopener')
})

test('an offline app has no window to open', async () => {
  serve({ archived_at: '2026-11-18T00:00:00.000Z' })
  render(<LaunchedApp workspace={1} space={2} app={1} writable />)
  await screen.findByText(/This app is offline/)
  expect(screen.queryByRole('button', { name: 'Open in a new window' })).toBeNull()
  expect(screen.queryByRole('link', { name: 'Open in a new tab' })).toBeNull()
})

test('standalone, an offline app or one that fails to load still has the way to taskto', async () => {
  serve({ archived_at: '2026-11-18T00:00:00.000Z' })
  const { unmount } = render(<LaunchedApp standalone workspace={1} space={2} app={1} writable />)
  await screen.findByText(/This app is offline/)
  expect(screen.getByRole('link', { name: 'Open in taskto' }).getAttribute('href')).toBe('/w/1/apps/2-1')
  expect(screen.queryByRole('button', { name: 'Records and history' })).toBeNull()
  unmount()
  cleanup()

  fakeFetch({})
  render(<LaunchedApp standalone workspace={1} space={2} app={1} writable />)
  await screen.findByRole('alert')
  expect(screen.getByRole('link', { name: 'Open in taskto' }).getAttribute('href')).toBe('/w/1/apps/2-1')
})
