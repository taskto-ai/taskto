// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { AvailableApp, HomeItem, HomeView, Me } from '../api'
import { beginSession, endSession } from '../session'
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
  ).toEqual(['Home', 'Conversations', 'Knowledge base', 'Boards', 'Apps', 'Notifications', 'Settings'])
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
  // After the sections and the inbox, in a group of their own, never among them; Settings stays at
  // the sidebar's foot.
  expect(nav.compareDocumentPosition(pinned) & Node.DOCUMENT_POSITION_CONTAINED_BY).toBeTruthy()
  const links = within(nav).getAllByRole('link')
  expect(links.indexOf(link)).toBe(links.findIndex((a) => a.getAttribute('href') === '/w/1/notifications') + 1)
  expect(links.at(-1)?.getAttribute('href')).toBe('/w/1/settings')
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
  // Setting a default is in the row's "more" menu, named for what it acts on.
  const more = () => screen.findByRole('button', { name: 'More actions for the Spending widget' })
  fireEvent.click(await more())
  fireEvent.click(await screen.findByRole('menuitem', { name: 'Set Spending as the space’s default' }))
  // Once set, the row says so.
  await screen.findByText('Space default')
  fireEvent.click(await more())
  fireEvent.click(await screen.findByRole('menuitem', { name: 'Stop Spending being the space’s default' }))
  await waitFor(() => expect(screen.queryByText('Space default')).toBeNull())
  fireEvent.click(await more())
  await screen.findByRole('menuitem', { name: 'Set Spending as the space’s default' })
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

test('in customize mode a widget of one’s own has named move and remove buttons; a move sends the new order, and neither end can be passed', async () => {
  const view = home({
    widgets: [
      widgetItem({ id: 10, widget_id: 'a', widget: { id: 'a', title: 'First', kind: 'shortcut' } }),
      widgetItem({ id: 11, widget_id: 'b', widget: { id: 'b', title: 'Second', kind: 'shortcut' } }),
    ],
  })
  const calls = fakeFetch({
    'GET /api/workspaces/1/home': { body: view },
    'GET /api/workspaces/1/home/available': { body: { apps: [] } },
    'PUT /api/workspaces/1/home/order': { status: 204 },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByRole('article', { name: 'First' })
  // Out of customize mode, a widget has no controls.
  expect(screen.queryByRole('button', { name: 'Move First later' })).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: 'Customize' }))
  const first = screen.getByRole('article', { name: 'First' })
  // The first can't go earlier, the last can't go later.
  expect((within(first).getByRole('button', { name: 'Move First earlier' }) as HTMLButtonElement).disabled).toBe(true)
  const second = screen.getByRole('article', { name: 'Second' })
  expect((within(second).getByRole('button', { name: 'Move Second later' }) as HTMLButtonElement).disabled).toBe(true)
  // A button, so Enter and Space press it; the name says which widget it moves.
  const later = within(first).getByRole('button', { name: 'Move First later' })
  later.focus()
  fireEvent.click(later)
  await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true))
  expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ kind: 'widget', ids: [11, 10] })
  // The remove control is an icon with the widget's name.
  expect(within(first).getByRole('button', { name: 'Remove First' })).toBeTruthy()
})

test('an empty Home offers the first step: adding a widget when an app offers one, the apps otherwise', async () => {
  fakeFetch({
    'GET /api/workspaces/1/home': { body: home() },
    'GET /api/workspaces/1/home/available': { body: { apps: [offered] } },
  })
  const { unmount } = render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByText('Nothing on your Home yet')
  fireEvent.click(await screen.findByRole('button', { name: 'Add widgets' }))
  // That opens customize mode, where the app's widget is offered, and the keyboard, whose button is
  // gone with the empty state, is on what to add.
  expect(await screen.findByRole('button', { name: 'Add Spending to Home' })).toBeTruthy()
  expect(document.activeElement).toBe(screen.getByRole('heading', { name: 'Add from your apps' }))
  expect(screen.getByRole('button', { name: 'Done' }).getAttribute('aria-expanded')).toBe('true')
  unmount()

  fakeFetch({
    'GET /api/workspaces/1/home': { body: home() },
    'GET /api/workspaces/1/home/available': { body: { apps: [] } },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  const apps = await screen.findByRole('link', { name: 'Open Apps' })
  expect(apps.getAttribute('href')).toBe('/w/1/apps')
  expect(screen.queryByRole('button', { name: 'Add widgets' })).toBeNull()
})

test('while Home loads it shows the shape of widgets, and says it is loading', async () => {
  let answer: (() => void) | undefined
  fakeFetch({
    'GET /api/workspaces/1/home': async () => {
      await new Promise<void>((resolve) => (answer = resolve))
      return { body: home() }
    },
    'GET /api/workspaces/1/home/available': { body: { apps: [] } },
  })
  const { container } = render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByRole('heading', { name: 'Home', level: 1 })
  await waitFor(() => expect(answer).toBeDefined())
  expect(container.querySelector('[aria-busy="true"]')).toBeTruthy()
  expect(screen.getByText('Loading…')).toBeTruthy()
  answer?.()
  await screen.findByText('Nothing on your Home yet')
  expect(container.querySelector('[aria-busy="true"]')).toBeNull()
})

test('an app’s offer says what is on Home already: Added, with a way to take it off', async () => {
  fakeFetch({
    'GET /api/workspaces/1/home': { body: home({ widgets: [widgetItem()], pins: [pinItem()] }) },
    'GET /api/workspaces/1/home/available': { body: { apps: [offered] } },
    'GET /api/workspaces/1/spaces/2/apps/1/surface?widget=spend': { body: spendSurface },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Customize' }))
  const group = await screen.findByRole('list', { name: 'Expenses' })
  const rows = within(group).getAllByRole('listitem')
  expect(within(rows[0]).getByText('Added')).toBeTruthy()
  // Short on screen, named in full: the visible word is the start of the name.
  const take = within(rows[0]).getByRole('button', { name: 'Remove Spending' })
  expect(take.textContent).toBe('Remove')
  expect(within(rows[1]).getByText('Pinned')).toBeTruthy()
  expect(within(rows[1]).getByRole('button', { name: 'Unpin Spend' }).textContent).toBe('Unpin')
})

// Two widgets of the person's own, that the server moves when asked.
function movable() {
  let order = [10, 11]
  const make = (id: number, title: string) =>
    widgetItem({ id, widget_id: `w${id}`, widget: { id: `w${id}`, title, kind: 'shortcut' } })
  const byId: Record<number, HomeItem> = { 10: make(10, 'First'), 11: make(11, 'Second') }
  // Every request for Home held while `hold` is on (the page's and the menu's pins), until released.
  const held: (() => void)[] = []
  let hold = false
  const calls = fakeFetch({
    'GET /api/workspaces/1/home': async () => {
      if (hold) await new Promise<void>((resolve) => held.push(resolve))
      return { body: home({ widgets: order.map((id) => byId[id]) }) }
    },
    'GET /api/workspaces/1/home/available': { body: { apps: [] } },
    'PUT /api/workspaces/1/home/order': (call) => {
      order = (call.body as { ids: number[] }).ids
      return { status: 204 }
    },
    'DELETE /api/workspaces/1/home/items/10': () => {
      order = order.filter((id) => id !== 10)
      return { status: 204 }
    },
  })
  return {
    calls,
    holdNext: () => {
      hold = true
    },
    release: () => {
      hold = false
      for (const go of held.splice(0)) go()
    },
  }
}

test('a move keeps Home shown while it reloads, keeps the keyboard on the button, and says where the widget went', async () => {
  const server = movable()
  render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByRole('article', { name: 'First' })
  fireEvent.click(screen.getByRole('button', { name: 'Customize' }))
  const later = screen.getByRole('button', { name: 'Move First later' })
  later.focus()
  server.holdNext()
  fireEvent.click(later)
  // While Home loads again, what was shown stays: no skeleton, the customize panel still open.
  const said = await screen.findByText('First moved to position 2 of 2')
  expect(said.closest('[role="status"]')).toBeTruthy()
  expect(screen.getByRole('article', { name: 'First' })).toBeTruthy()
  expect(screen.getByRole('heading', { name: 'Add from your apps' })).toBeTruthy()
  expect(screen.queryByText('Loading…')).toBeNull()
  server.release()
  // Last now, so "later" is off: the keyboard is on its "earlier" button.
  await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Move First earlier' })))
  expect(screen.getAllByRole('article').map((a) => a.querySelector('h2')?.textContent)).toEqual(['Second', 'First'])
})

test('a move that leaves the widget able to go the same way keeps the keyboard on the same button', async () => {
  let order = [10, 11, 12]
  const byId: Record<number, HomeItem> = Object.fromEntries(
    ['First', 'Second', 'Third'].map((title, n) => [
      10 + n,
      widgetItem({ id: 10 + n, widget_id: `w${n}`, widget: { id: `w${n}`, title, kind: 'shortcut' } }),
    ]),
  )
  fakeFetch({
    'GET /api/workspaces/1/home': () => ({ body: home({ widgets: order.map((id) => byId[id]) }) }),
    'GET /api/workspaces/1/home/available': { body: { apps: [] } },
    'PUT /api/workspaces/1/home/order': (call) => {
      order = (call.body as { ids: number[] }).ids
      return { status: 204 }
    },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByRole('article', { name: 'First' })
  fireEvent.click(screen.getByRole('button', { name: 'Customize' }))
  fireEvent.click(screen.getByRole('button', { name: 'Move First later' }))
  await screen.findByText('First moved to position 2 of 3')
  await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Move First later' })))
})

test('removing a widget puts the keyboard on the next one’s remove button, and says so', async () => {
  movable()
  render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByRole('article', { name: 'First' })
  fireEvent.click(screen.getByRole('button', { name: 'Customize' }))
  fireEvent.click(screen.getByRole('button', { name: 'Remove First' }))
  await screen.findByText('First removed from Home')
  await waitFor(() => expect(screen.queryByRole('article', { name: 'First' })).toBeNull())
  expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Remove Second' }))
})

test('someone who may not set a space’s defaults is not offered them', async () => {
  fakeFetch({
    'GET /api/workspaces/1/home': { body: home() },
    'GET /api/workspaces/1/home/available': {
      body: { apps: [{ ...offered, space_id: 3, space_name: 'Team', space_kind: 'shared', can_set_defaults: false }] },
    },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Customize' }))
  await screen.findByRole('button', { name: 'Add Spending to Home' })
  expect(screen.queryByRole('button', { name: /^More actions for/ })).toBeNull()
  expect(screen.queryByText(/Only people who may publish the app set one/)).toBeNull()
})

test('a widget and a pin of the same name have “more” menus named apart', async () => {
  fakeFetch({
    'GET /api/workspaces/1/home': { body: home() },
    'GET /api/workspaces/1/home/available': {
      body: {
        apps: [
          {
            ...offered,
            space_id: 3,
            space_kind: 'shared',
            space_name: 'Team',
            can_set_defaults: true,
            widgets: [{ id: 'eq', title: 'Equipment', kind: 'summaries' }],
            pin: { label: 'Equipment' },
          },
        ],
      },
    },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Customize' }))
  expect(await screen.findByRole('button', { name: 'More actions for the Equipment widget' })).toBeTruthy()
  expect(screen.getByRole('button', { name: 'More actions for the Equipment pin' })).toBeTruthy()
})

test('a reload started before a move finishes doesn’t settle the keyboard; the reload after it does', async () => {
  let order = [10, 11]
  const byId: Record<number, HomeItem> = Object.fromEntries(
    ['First', 'Second'].map((title, n) => [
      10 + n,
      widgetItem({ id: 10 + n, widget_id: `w${n}`, widget: { id: `w${n}`, title, kind: 'shortcut' } }),
    ]),
  )
  const held: (() => void)[] = []
  let hold = false
  let finishMove: (() => void) | undefined
  fakeFetch({
    'GET /api/workspaces/1/home': async () => {
      // What the server has when asked, answered when released.
      const snapshot = home({ widgets: order.map((id) => byId[id]) })
      if (hold) await new Promise<void>((resolve) => held.push(resolve))
      return { body: snapshot }
    },
    'GET /api/workspaces/1/home/available': { body: { apps: [] } },
    'PUT /api/workspaces/1/home/order': async (call) => {
      await new Promise<void>((resolve) => (finishMove = resolve))
      order = (call.body as { ids: number[] }).ids
      return { status: 204 }
    },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByRole('article', { name: 'First' })
  fireEvent.click(screen.getByRole('button', { name: 'Customize' }))
  const later = screen.getByRole('button', { name: 'Move First later' })
  later.focus()
  fireEvent.click(later)
  await waitFor(() => expect(finishMove).toBeDefined())
  // A change made elsewhere starts a reload while the move is on its way; it answers first.
  hold = true
  act(() => homeChanged())
  await waitFor(() => expect(held.length).toBeGreaterThan(0))
  act(() => {
    for (const go of held.splice(0)) go()
  })
  // That answer says nothing of the move, which hasn't finished: the keyboard stays where it is. (A
  // reload started before the move finishes is dropped once the move's own reload starts, so this
  // is the window where an early answer could land.)
  await screen.findByRole('article', { name: 'First' })
  expect(document.activeElement).toBe(later)
  // Then the move finishes, and Home loads what it did.
  hold = false
  act(() => finishMove?.())
  await screen.findByText('First moved to position 2 of 2')
  await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Move First earlier' })))
})

test('a Home change answered after its session ended tells nothing to whatever shows Home next', async () => {
  let answer: () => void = () => {}
  let deleted = false
  const calls = fakeFetch({
    'GET /api/workspaces/1/home': () => ({
      body: home({
        widgets: deleted ? [] : [widgetItem({ id: 10, widget: { id: 'w0', title: 'First', kind: 'shortcut' } })],
      }),
    }),
    'GET /api/workspaces/1/home/available': { body: { apps: [] } },
    'DELETE /api/workspaces/1/home/items/10': () =>
      new Promise((done) => {
        answer = () => {
          deleted = true
          done({ status: 204 })
        }
      }),
  })
  beginSession(1)
  render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByRole('article', { name: 'First' })
  fireEvent.click(screen.getByRole('button', { name: 'Customize' }))
  fireEvent.click(screen.getByRole('button', { name: 'Remove First' }))
  await waitFor(() => expect(calls.some((c) => c.method === 'DELETE')).toBe(true))
  // The first person signs out and the next signs in, before the removal is answered.
  endSession()
  beginSession(2)
  const loads = () => calls.filter((c) => c.method === 'GET' && c.path === '/api/workspaces/1/home').length
  const before = loads()
  await act(async () => answer())
  await new Promise((r) => setTimeout(r, 20))
  // No Home of anyone's is told to load again by the first person's late answer.
  expect(loads()).toBe(before)
})
