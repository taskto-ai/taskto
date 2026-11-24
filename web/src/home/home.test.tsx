// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { AvailableApp, HomeItem, HomeView, Me } from '../api'
import { Shell } from '../shell/Shell'
import { fakeFetch } from '../test/fakes'
import { PinnedNav } from './PinnedNav'
import { homeChanged } from './store'

const me: Me = {
  user: { id: 1, email: 'ann@example.com', name: 'Ann' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
  can_create_workspace: false,
  is_operator: false,
}

const CATALOG = 'https://taskto.ai/catalogs/apps/v1.json'
// What the server answers for the summaries widget "spend" of app 1 (internal/apps WidgetSurface).
const spendSurface = [
  { version: 'v0.9', createSurface: { surfaceId: 'app-1', catalogId: CATALOG } },
  {
    version: 'v0.9',
    updateComponents: {
      surfaceId: 'app-1',
      components: [
        { id: 'root', component: 'Column', children: ['stat0'] },
        { id: 'stat0', component: 'Stat', label: 'Total spent', value: { path: '/total' } },
      ],
    },
  },
  { version: 'v0.9', updateDataModel: { surfaceId: 'app-1', path: '/', value: { total: '4.50' } } },
]
const release = {
  number: 3,
  definition_version: 2,
  requires: ['component.Column', 'component.Stat', 'slot.home_widgets.v1'],
}

const widgetItem = (over: Partial<HomeItem> = {}): HomeItem => ({
  id: 10,
  source: 'personal',
  kind: 'widget',
  space_id: 2,
  app_id: 1,
  app_name: 'Expenses',
  widget_id: 'spend',
  widget: {
    id: 'spend',
    title: 'Spending',
    kind: 'summaries',
    size: 'medium',
    summaries: [{ summary: 'total', label: 'Total spent' }],
  },
  state: 'ready',
  release,
  ...over,
})
const pinItem = (over: Partial<HomeItem> = {}): HomeItem => ({
  id: 11,
  source: 'personal',
  kind: 'pin',
  space_id: 2,
  app_id: 1,
  app_name: 'Expenses',
  label: 'Spend',
  state: 'ready',
  release,
  ...over,
})
const home = (over: Partial<HomeView> = {}): HomeView => ({
  widgets: [],
  pins: [],
  hidden: [],
  limits: { widgets: 24, pins: 20 },
  ...over,
})
const offered: AvailableApp = {
  space_id: 2,
  space_name: 'Private',
  space_kind: 'private',
  app_id: 1,
  name: 'Expenses',
  widgets: [{ id: 'spend', title: 'Spending', kind: 'summaries', size: 'medium' }],
  pin: { label: 'Spend' },
  can_set_defaults: false,
  defaults: [],
}

beforeEach(() => {
  window.history.replaceState(null, '', '/w/1/home')
  localStorage.clear()
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

test('a person pins an app and places a widget, and both show: the widget on Home, the pin in the menu', async () => {
  let view = home()
  let next = 10
  const calls = fakeFetch({
    'GET /api/workspaces/1/home': () => ({ body: view }),
    'GET /api/workspaces/1/home/available': { body: { apps: [offered] } },
    'POST /api/workspaces/1/home/items': (call) => {
      const body = call.body as { kind: 'widget' | 'pin'; widget?: string }
      const item = body.kind === 'widget' ? widgetItem({ id: next++ }) : pinItem({ id: next++ })
      view =
        body.kind === 'widget' ? { ...view, widgets: [...view.widgets, item] } : { ...view, pins: [...view.pins, item] }
      return { status: 201, body: item }
    },
    'DELETE /api/workspaces/1/home/items/11': () => {
      view = { ...view, pins: [] }
      return { status: 204 }
    },
    'GET /api/workspaces/1/spaces/2/apps/1/surface?widget=spend': { body: spendSurface },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByRole('heading', { name: 'Home', level: 1 })
  await screen.findByText(/Nothing on your Home yet/)
  // The navigation lists the platform's pages, and "Home" is one of them.
  const nav = screen.getByRole('navigation', { name: 'Sections' })
  expect(
    within(nav)
      .getAllByRole('link')
      .map((a) => a.textContent),
  ).toEqual(['Home', 'Conversations', 'Knowledge base', 'Boards', 'Apps', 'Settings'])
  expect(within(nav).queryByText('Pinned apps')).toBeNull()

  fireEvent.click(screen.getByRole('button', { name: 'Customize' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Add Spending to Home' }))
  // The widget is the app's own view: its summary comes from the app's surface.
  const widget = await screen.findByRole('article', { name: 'Spending' })
  await within(widget).findByText('4.50')
  expect(within(widget).getByText('Total spent')).toBeTruthy()
  expect(within(widget).getByRole('link', { name: 'Open Expenses' }).getAttribute('href')).toBe('/w/1/apps/2-1')
  expect(calls.find((c) => c.method === 'POST')?.body).toEqual({
    kind: 'widget',
    space_id: 2,
    app_id: 1,
    widget: 'spend',
  })

  fireEvent.click(await screen.findByRole('button', { name: 'Pin Spend' }))
  const pinned = await screen.findByRole('region', { name: 'Pinned apps' })
  const link = within(pinned).getByRole('link', { name: 'Spend' })
  expect(link.getAttribute('href')).toBe('/w/1/apps/2-1')
  // After the platform's pages, never among them.
  expect(
    nav.compareDocumentPosition(pinned) & Node.DOCUMENT_POSITION_CONTAINED_BY &&
      within(nav).getAllByRole('link').at(-2)?.textContent,
  ).toBe('Settings')
  expect(calls.filter((c) => c.method === 'POST').at(-1)?.body).toEqual({ kind: 'pin', space_id: 2, app_id: 1 })

  // Unpinning takes it out of the menu again, in this page and the menu alike.
  fireEvent.click(await screen.findByRole('button', { name: 'Unpin Spend' }))
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Pinned apps' })).toBeNull())
  expect(screen.getByRole('article', { name: 'Spending' })).toBeTruthy()
})

test('a pin opens its app on the screen it names', async () => {
  window.history.replaceState(null, '', '/w/1/conversations')
  fakeFetch({
    'GET /api/workspaces/1/home': { body: home({ pins: [pinItem({ screen: 'new' })] }) },
    'GET /api/workspaces/1/conversations': { body: { conversations: [] } },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  const link = await screen.findByRole('link', { name: 'Spend' })
  expect(link.getAttribute('href')).toBe('/w/1/apps/2-1-new')
})

test('an app that is offline, unpublished or no longer offers the widget says so, with no data; one that needs a newer client asks for an update', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/home': {
      body: home({
        widgets: [
          widgetItem({ id: 20, state: 'unavailable', reason: 'offline', widget: undefined, release: undefined }),
          widgetItem({ id: 21, state: 'unavailable', reason: 'unpublished', widget: undefined, release: undefined }),
          widgetItem({ id: 22, state: 'unavailable', reason: 'removed', widget: undefined, release: undefined }),
          widgetItem({ id: 23, release: { number: 4, definition_version: 9, requires: ['component.Hologram'] } }),
        ],
        pins: [pinItem({ state: 'unavailable', reason: 'offline', label: undefined, release: undefined })],
      }),
    },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByText('Expenses is offline, so this widget is unavailable.')
  expect(screen.getByText('Expenses isn’t published yet, so this widget is unavailable.')).toBeTruthy()
  expect(screen.getByText('Expenses no longer offers this widget.')).toBeTruthy()
  // The widget that needs more than this client has isn't drawn, and says why.
  expect(await screen.findByText(/definition_version\.9, component\.Hologram/)).toBeTruthy()
  // Nothing of any of them was asked for, and an unavailable pin is not in the menu.
  expect(calls.some((c) => c.path.includes('/surface'))).toBe(false)
  expect(screen.queryByRole('region', { name: 'Pinned apps' })).toBeNull()
})

test('a space’s default can be hidden, and shown again', async () => {
  let view = home({ widgets: [widgetItem({ id: undefined, default_id: 5, source: 'default' })] })
  const calls = fakeFetch({
    'GET /api/workspaces/1/home': () => ({ body: view }),
    'GET /api/workspaces/1/home/available': { body: { apps: [] } },
    'GET /api/workspaces/1/spaces/2/apps/1/surface?widget=spend': { body: spendSurface },
    'PUT /api/workspaces/1/home/hidden/5': () => {
      view = home({ hidden: view.widgets })
      return { status: 204 }
    },
    'DELETE /api/workspaces/1/home/hidden/5': () => {
      view = home({ widgets: view.hidden })
      return { status: 204 }
    },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByRole('article', { name: 'Spending' })
  expect(screen.getByText('From Expenses · set for the space')).toBeTruthy()
  fireEvent.click(screen.getByRole('button', { name: 'Customize' }))
  // A default can't be removed by a member, or moved: only hidden.
  expect(screen.queryByRole('button', { name: 'Remove Spending' })).toBeNull()
  fireEvent.click(await screen.findByRole('button', { name: 'Hide Spending' }))
  await screen.findByRole('button', { name: 'Show Spending again' })
  expect(screen.queryByRole('article', { name: 'Spending' })).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: 'Show Spending again' }))
  await screen.findByRole('article', { name: 'Spending' })
  expect(calls.filter((c) => c.method !== 'GET').map((c) => `${c.method} ${c.path}`)).toEqual([
    'PUT /api/workspaces/1/home/hidden/5',
    'DELETE /api/workspaces/1/home/hidden/5',
  ])
})

test('those who may publish an app set its widgets and pin as defaults of its space, and take them back', async () => {
  let defaults: AvailableApp['defaults'] = []
  const calls = fakeFetch({
    'GET /api/workspaces/1/home': { body: home() },
    'GET /api/workspaces/1/home/available': () => ({
      body: {
        apps: [{ ...offered, space_id: 3, space_name: 'Team', space_kind: 'shared', can_set_defaults: true, defaults }],
      },
    }),
    'POST /api/workspaces/1/spaces/3/home/defaults': () => {
      defaults = [{ id: 9, kind: 'widget', widget: 'spend' }]
      return { status: 201, body: widgetItem({ id: undefined, default_id: 9, source: 'default', space_id: 3 }) }
    },
    'DELETE /api/workspaces/1/spaces/3/home/defaults/9': () => {
      defaults = []
      return { status: 204 }
    },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Customize' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Set Spending as the space’s default' }))
  fireEvent.click(await screen.findByRole('button', { name: 'Stop Spending being the space’s default' }))
  await screen.findByRole('button', { name: 'Set Spending as the space’s default' })
  expect(calls.filter((c) => c.method !== 'GET').map((c) => [c.method, c.path, c.body])).toEqual([
    ['POST', '/api/workspaces/1/spaces/3/home/defaults', { kind: 'widget', app_id: 1, widget: 'spend' }],
    ['DELETE', '/api/workspaces/1/spaces/3/home/defaults/9', undefined],
  ])
  // Someone who can't set defaults isn't offered them.
  expect(screen.getByText(/Only people who may publish the app set one/)).toBeTruthy()
})

test('a failing request for Home leaves the menu and the page standing, with the error', async () => {
  fakeFetch({ 'GET /api/workspaces/1/home': { status: 500, body: { error: 'boom' } } })
  render(<Shell me={me} onLogout={() => {}} />)
  expect((await screen.findByRole('alert')).textContent).toBe('boom')
  expect(
    within(screen.getByRole('navigation', { name: 'Sections' })).getByRole('link', { name: 'Settings' }),
  ).toBeTruthy()
})

test('the pins follow a change made elsewhere', async () => {
  let view = home()
  fakeFetch({ 'GET /api/workspaces/1/home': () => ({ body: view }) })
  render(<PinnedNav workspace={1} path="/w/1/home" />)
  await waitFor(() => expect(screen.queryByRole('region')).toBeNull())
  view = home({ pins: [pinItem()] })
  act(() => homeChanged())
  await screen.findByRole('link', { name: 'Spend' })
})
