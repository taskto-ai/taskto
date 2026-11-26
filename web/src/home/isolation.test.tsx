// @vitest-environment jsdom

// What a contribution can't do: break the shell, Home or the other widgets, however it fails.

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { HomeItem, HomeView, Me } from '../api'
import { Shell } from '../shell/Shell'
import { fakeFetch } from '../test/fakes'

// A widget whose renderer throws, as one with a broken definition or a bug in the renderer would.
vi.mock('../apps/transport', () => ({
  httpTransport: () => ({ load: async () => [], act: async () => [] }),
}))
vi.mock('../apps/AppView', () => ({
  AppView: ({ label }: { label: string }) => {
    if (label === 'Broken') throw new Error('the renderer failed')
    return <p>{`drawn: ${label}`}</p>
  },
}))

const me: Me = {
  user: { id: 1, email: 'ann@example.com', name: 'Ann' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' }],
  can_create_workspace: false,
  is_operator: false,
}
const release = { number: 1, definition_version: 2, requires: [] }
const widget = (id: number, title: string, over: Record<string, unknown> = {}): HomeItem =>
  ({
    id,
    source: 'personal',
    kind: 'widget',
    space_id: 2,
    app_id: id,
    app_name: `App ${id}`,
    widget_id: `w${id}`,
    widget: { id: `w${id}`, title, kind: 'screen', screen: 'main' },
    state: 'ready',
    release,
    ...over,
  }) as HomeItem
const view = (over: Record<string, unknown>) =>
  ({ widgets: [], pins: [], hidden: [], limits: { widgets: 24, pins: 20 }, ...over }) as HomeView

beforeEach(() => {
  window.history.replaceState(null, '', '/w/1/home')
  localStorage.clear()
  // React reports a caught render error to the console; the tests make them on purpose.
  vi.spyOn(console, 'error').mockImplementation(() => {})
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

const settingsLink = () =>
  within(screen.getByRole('navigation', { name: 'Sections' })).getByRole('link', { name: 'Settings' })

test('a widget that throws is replaced by a message; the other widgets, Home and the menu carry on', async () => {
  fakeFetch({
    'GET /api/workspaces/1/home': {
      body: view({
        widgets: [
          widget(1, 'Broken'),
          widget(2, 'Fine'),
          // An answer with the wrong shape (a title that isn't text) throws while drawing too.
          widget(3, 'x', { widget: { id: 'w3', title: { not: 'text' }, kind: 'screen', screen: 'main' } }),
        ],
      }),
    },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  const fine = await screen.findByRole('article', { name: 'Fine' })
  // The renderer is loaded when the first widget is drawn (LazyAppView): a moment later.
  expect(await within(fine).findByText('drawn: Fine')).toBeTruthy()
  await waitFor(() => expect(screen.getAllByText('This widget couldn’t be shown.')).toHaveLength(2))
  // The page, and the shell around it.
  expect(screen.getByRole('heading', { name: 'Home', level: 1 })).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Customize' })).toBeTruthy()
  // A failed widget can still be taken off Home.
  expect(screen.getAllByRole('button', { name: 'Remove' })).toHaveLength(2)
  fireEvent.click(settingsLink())
  expect(window.location.pathname).toBe('/w/1/settings')
})

test('a pin with a malformed label can’t take the menu down', async () => {
  fakeFetch({
    'GET /api/workspaces/1/home': {
      body: view({
        pins: [
          {
            id: 1,
            source: 'personal',
            kind: 'pin',
            space_id: 2,
            app_id: 1,
            app_name: 'X',
            label: { evil: 1 },
            state: 'ready',
            release,
          },
        ],
      }),
    },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByRole('heading', { name: 'Home', level: 1 })
  // The sections are all there, and no pins section.
  const nav = screen.getByRole('navigation', { name: 'Sections' })
  expect(within(nav).getAllByRole('link')).toHaveLength(7)
  expect(within(nav).queryByRole('region')).toBeNull()
  fireEvent.click(settingsLink())
  expect(window.location.pathname).toBe('/w/1/settings')
})

test('a page that fails to draw leaves the shell usable, and another page is drawn again', async () => {
  fakeFetch({
    // Not even a list: Home itself throws while drawing it.
    'GET /api/workspaces/1/home': { body: { widgets: 'oops', pins: 'oops', hidden: [] } },
    'GET /api/workspaces/1/conversations': { body: { conversations: [] } },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  const failed = await screen.findByRole('alert')
  expect(failed.textContent).toContain('This page couldn’t be shown. The menu and your settings still work.')
  // And what to do: reload.
  expect(within(failed).getByRole('button', { name: 'Reload' })).toBeTruthy()
  const nav = screen.getByRole('navigation', { name: 'Sections' })
  expect(within(nav).getAllByRole('link')).toHaveLength(7)
  fireEvent.click(within(nav).getByRole('link', { name: 'Conversations' }))
  await screen.findByRole('heading', { name: 'Conversations' })
  expect(screen.queryByText(/This page couldn’t be shown/)).toBeNull()
})

test('a pin can’t stand in for a platform page: pins come after the sections and the inbox, in a group of their own', async () => {
  fakeFetch({
    'GET /api/workspaces/1/home': {
      body: view({
        pins: [
          // A label that imitates a platform page, or markup, is only text.
          {
            id: 1,
            source: 'personal',
            kind: 'pin',
            space_id: 2,
            app_id: 1,
            app_name: 'X',
            label: 'Settings',
            state: 'ready',
            release,
          },
          {
            id: 2,
            source: 'personal',
            kind: 'pin',
            space_id: 2,
            app_id: 2,
            app_name: 'Y',
            label: '<img src=x onerror=alert(1)>',
            state: 'ready',
            release,
          },
        ],
      }),
    },
  })
  const { container } = render(<Shell me={me} onLogout={() => {}} />)
  const pins = await screen.findByRole('region', { name: 'Pinned apps' })
  const nav = screen.getByRole('navigation', { name: 'Sections' })
  const platform = within(nav)
    .getAllByRole('link')
    .filter((a) => !pins.contains(a))
  expect(platform.map((a) => a.textContent)).toEqual([
    'Home',
    'Conversations',
    'Knowledge base',
    'Boards',
    'Apps',
    'Notifications',
    'Settings',
  ])
  // The real Settings link still goes to settings; the pin of that name goes to its app.
  expect(platform.at(-1)?.getAttribute('href')).toBe('/w/1/settings')
  expect(within(pins).getByRole('link', { name: 'Settings' }).getAttribute('href')).toBe('/w/1/apps/2-1')
  expect(container.querySelector('img')).toBeNull()
  expect(within(pins).getByRole('link', { name: '<img src=x onerror=alert(1)>' })).toBeTruthy()
})
