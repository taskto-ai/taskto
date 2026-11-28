// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import type { Me } from '../api'
import { type BeforeLeave, navigate, setFocusedPlace, usePath } from '../router'
import { beginSession } from '../session'
import { Shell } from '../shell/Shell'
import { type Call, fakeFetch } from '../test/fakes'
import { resetToasts, Toaster } from '../ui/Toast'
import { Desk } from './Desk'
import { SAVE_DELAY_MS } from './useDesk'

const guards = vi.hoisted(() => ({ ask: null as BeforeLeave | null }))

vi.mock('../shell/SectionPage', async () => {
  const { useBeforeLeave } = await import('../router')
  return {
    Loading: () => null,
    SectionPage: ({ route }: { route: { section: string; item?: string } }) => {
      useBeforeLeave(guards.ask)
      return (
        <div data-testid="page">
          {route.section}
          {route.item ? `:${route.item}` : ''}
        </div>
      )
    },
  }
})

const me: Me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [
    { id: 1, name: 'Home', role: 'admin' },
    { id: 2, name: 'Team', role: 'member' },
  ],
  can_create_workspace: false,
  is_operator: false,
}

const two = {
  v: 1,
  focus: 'ga',
  root: {
    split: 'row',
    sizes: [0.5, 0.5],
    children: [
      {
        group: 'ga',
        active: 'ta',
        tabs: [
          { id: 'ta', path: '/w/1/conversations' },
          { id: 'tb', path: '/w/1/knowledge' },
        ],
      },
      { group: 'gb', active: 'tc', tabs: [{ id: 'tc', path: '/w/1/boards' }] },
    ],
  },
}

const other = { v: 1, focus: 'gz', root: { group: 'gz', active: 'tz', tabs: [{ id: 'tz', path: '/w/1/apps' }] } }

const deskOf = (layout: unknown, version = 3) => ({
  id: 9,
  name: '',
  layout,
  version,
  updated_at: '2026-01-01T00:00:00Z',
})

function Harness() {
  const path = usePath()
  return <Desk me={me} workspace={me.workspaces[0]} path={path} failed={null} />
}

function Landing() {
  const url = usePath()
  return <Desk me={me} workspace={me.workspaces[0]} path={url === '/' ? '/w/1/boards' : url} failed={null} />
}

const desks = (layout: unknown, version = 3) => ({
  'GET /api/workspaces/1/desks/main': { body: deskOf(layout, version) },
  'PUT /api/workspaces/1/desks/main/layout': (call: { body: unknown }) => {
    const sent = call.body as { version: number; layout: unknown }
    return { body: deskOf(sent.layout, sent.version + 1) }
  },
})

const pages = () => screen.getAllByTestId('page').map((p) => p.textContent)
const puts = (calls: Call[]) => calls.filter((c) => c.method === 'PUT')

async function mount(layout: unknown, at: string, routes: Record<string, unknown> = {}) {
  window.history.replaceState(null, '', at)
  const calls = fakeFetch({ ...desks(layout), ...routes } as Parameters<typeof fakeFetch>[0])
  render(<Harness />)
  await screen.findAllByRole('tab')
  await waitFor(() => expect(calls.some((c) => c.method === 'GET')).toBe(true))
  await act(async () => {})
  return calls
}

async function wait(ms: number) {
  await act(async () => {
    vi.advanceTimersByTime(ms)
  })
  await act(async () => {})
}

beforeEach(() => {
  guards.ask = null
  beginSession(1)
  localStorage.clear()
  resetToasts()
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  setFocusedPlace(null)
})

test('a saved two-pane layout draws both panes, and only the active tab of each is mounted', async () => {
  await mount(two, '/w/1/conversations')
  expect(pages()).toEqual(['conversations', 'boards'])
  expect(screen.getAllByRole('tab')).toHaveLength(3)
  expect(screen.getAllByRole('tablist')).toHaveLength(2)
})

test('a sidebar link changes only the focused pane, and the layout is saved with it', async () => {
  const calls = await mount(two, '/w/1/conversations')
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
  act(() => navigate('/w/1/settings'))
  expect(pages()).toEqual(['settings', 'boards'])
  await wait(SAVE_DELAY_MS)
  const sent = puts(calls)
  expect(sent).toHaveLength(1)
  const layout = (sent[0].body as { layout: typeof two }).layout
  expect(layout.root.children[0]).toMatchObject({ tabs: [{ path: '/w/1/settings' }, { path: '/w/1/knowledge' }] })
  expect(layout.root.children[1]).toEqual(two.root.children[1])
})

test('pointing into the other pane moves the address to its page by replacing it', async () => {
  await mount(two, '/w/1/conversations')
  const replace = vi.spyOn(window.history, 'replaceState')
  const push = vi.spyOn(window.history, 'pushState')
  const other = screen.getAllByRole('region')[1]
  fireEvent.pointerDown(within(other).getByTestId('page'))
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/boards'))
  expect(replace).toHaveBeenCalled()
  expect(push).not.toHaveBeenCalled()
  act(() => navigate('/w/1/apps'))
  expect(pages()).toEqual(['conversations', 'apps'])
})

test('an address that is already a tab activates that tab instead of opening a second one', async () => {
  await mount(two, '/w/1/knowledge')
  expect(pages()).toEqual(['knowledge', 'boards'])
  act(() => navigate('/w/1/boards'))
  expect(pages()).toEqual(['knowledge', 'boards'])
  expect(screen.getAllByRole('tab')).toHaveLength(3)
  expect(window.location.pathname).toBe('/w/1/boards')
})

test('arriving with no address of a page shows the saved desk as it was and changes nothing', async () => {
  window.history.replaceState(null, '', '/')
  const calls = fakeFetch(desks(two))
  render(<Landing />)
  await screen.findAllByRole('tab')
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/conversations'))
  expect(pages()).toEqual(['conversations', 'boards'])
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
  await wait(SAVE_DELAY_MS * 2)
  expect(puts(calls)).toHaveLength(0)
})

test('a deep link goes to the focused tab', async () => {
  await mount(two, '/w/1/settings')
  expect(pages()).toEqual(['settings', 'boards'])
})

test('closing a tab with unsaved changes asks first and closes only when told to go', async () => {
  guards.ask = vi.fn()
  await mount(two, '/w/1/conversations')
  fireEvent.click(screen.getByRole('button', { name: 'Close Conversations' }))
  expect(guards.ask).toHaveBeenCalledTimes(1)
  expect(screen.getAllByRole('tab')).toHaveLength(3)
  act(() => (guards.ask as unknown as { mock: { calls: [() => void][] } }).mock.calls[0][0]())
  expect(screen.getAllByRole('tab')).toHaveLength(2)
  expect(pages()).toEqual(['knowledge', 'boards'])
})

test('several quick changes are saved once, 800 ms after the last, with the version read', async () => {
  const calls = await mount(two, '/w/1/conversations')
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
  fireEvent.click(screen.getByRole('tab', { name: 'Knowledge base' }))
  await wait(SAVE_DELAY_MS - 1)
  fireEvent.click(screen.getAllByRole('button', { name: 'New tab' })[0])
  await wait(SAVE_DELAY_MS - 1)
  expect(puts(calls)).toHaveLength(0)
  await wait(1)
  expect(puts(calls)).toHaveLength(1)
  expect((puts(calls)[0].body as { version: number }).version).toBe(3)
  fireEvent.click(screen.getAllByRole('button', { name: 'New tab' })[1])
  await wait(SAVE_DELAY_MS)
  expect(puts(calls)).toHaveLength(2)
  expect((puts(calls)[1].body as { version: number }).version).toBe(4)
})

test('a save that fails says so, keeps the change, and the next change sends both', async () => {
  let refuse = true
  const calls = await mount(two, '/w/1/conversations', {
    'PUT /api/workspaces/1/desks/main/layout': (call: { body: unknown }) =>
      refuse
        ? { status: 500, body: { error: 'down' } }
        : { body: deskOf((call.body as { layout: unknown }).layout, 4) },
  })
  render(<Toaster />)
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
  fireEvent.click(screen.getByRole('tab', { name: 'Knowledge base' }))
  await wait(SAVE_DELAY_MS)
  expect(puts(calls)).toHaveLength(1)
  expect(screen.getAllByText('Your desk layout could not be saved.')).not.toHaveLength(0)
  await wait(SAVE_DELAY_MS * 3)
  expect(puts(calls)).toHaveLength(1)
  refuse = false
  fireEvent.click(screen.getAllByRole('button', { name: 'New tab' })[0])
  await wait(SAVE_DELAY_MS)
  expect(puts(calls)).toHaveLength(2)
  const layout = (puts(calls)[1].body as { layout: typeof two }).layout
  expect(layout.root.children[0].tabs).toHaveLength(3)
  expect(layout.root.children[0].active).not.toBe('ta')
})

test('leaving the page sends a pending change at once, kept alive', async () => {
  const calls = await mount(two, '/w/1/conversations')
  fireEvent.click(screen.getByRole('tab', { name: 'Knowledge base' }))
  expect(puts(calls)).toHaveLength(0)
  act(() => {
    window.dispatchEvent(new Event('pagehide'))
  })
  await waitFor(() => expect(puts(calls)).toHaveLength(1))
  const init = vi.mocked(fetch).mock.calls.find(([, i]) => i?.method === 'PUT')?.[1]
  expect(init?.keepalive).toBe(true)
})

test('a conflict adopts the layout the server has and its version', async () => {
  const calls = await mount(two, '/w/1/conversations', {
    'PUT /api/workspaces/1/desks/main/layout': (call: { body: unknown }) =>
      (call.body as { version: number }).version === 3
        ? { status: 409, body: { error: 'changed', code: 'desk_changed', current: deskOf(other, 7) } }
        : { body: deskOf(other, 8) },
  })
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
  fireEvent.click(screen.getByRole('tab', { name: 'Knowledge base' }))
  await wait(SAVE_DELAY_MS)
  expect(pages()).toEqual(['apps'])
  expect(window.location.pathname).toBe('/w/1/apps')
  expect(puts(calls)).toHaveLength(1)
  fireEvent.click(screen.getByRole('button', { name: 'New tab' }))
  await wait(SAVE_DELAY_MS)
  expect((puts(calls)[1].body as { version: number }).version).toBe(7)
})

test('nothing is sent once another session has begun', async () => {
  const calls = await mount(two, '/w/1/conversations')
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
  fireEvent.click(screen.getByRole('tab', { name: 'Knowledge base' }))
  act(() => beginSession(2))
  await wait(SAVE_DELAY_MS)
  act(() => {
    window.dispatchEvent(new Event('pagehide'))
  })
  expect(puts(calls)).toHaveLength(0)
})

test('a pending change is sent when the desk goes away in the same session, as on a switch of workspace', async () => {
  const calls = await mount(two, '/w/1/conversations')
  fireEvent.click(screen.getByRole('tab', { name: 'Knowledge base' }))
  expect(puts(calls)).toHaveLength(0)
  cleanup()
  await waitFor(() => expect(puts(calls)).toHaveLength(1))
})

test('a pending change is not sent after the desk is gone and the session ended', async () => {
  const calls = await mount(two, '/w/1/conversations')
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
  fireEvent.click(screen.getByRole('tab', { name: 'Knowledge base' }))
  act(() => beginSession(2))
  cleanup()
  await wait(SAVE_DELAY_MS * 2)
  expect(puts(calls)).toHaveLength(0)
})

test('a desk that could not be loaded shows the address as one pane and is never overwritten', async () => {
  window.history.replaceState(null, '', '/w/1/boards')
  const calls = fakeFetch({ 'GET /api/workspaces/1/desks/main': { status: 500, body: { error: 'down' } } })
  render(<Harness />)
  await screen.findByRole('tab', { name: 'Boards' })
  expect(pages()).toEqual(['boards'])
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
  fireEvent.click(screen.getByRole('button', { name: 'New tab' }))
  await wait(SAVE_DELAY_MS * 2)
  act(() => {
    window.dispatchEvent(new Event('pagehide'))
  })
  expect(puts(calls)).toHaveLength(0)
})

test('a saved desk that names another workspace is not used', async () => {
  const foreign = {
    ...other,
    root: {
      group: 'gz',
      active: 'tz',
      tabs: [
        { id: 'tz', path: '/w/1/apps' },
        { id: 'ty', path: '/w/2/apps' },
      ],
    },
  }
  const calls = await mount(foreign, '/w/1/boards')
  expect(pages()).toEqual(['boards'])
  expect(screen.getAllByRole('tab')).toHaveLength(1)
  expect(puts(calls)).toHaveLength(0)
})

test('the shell draws the desk on a wide screen and the single page below it', async () => {
  const query = (wide: boolean) =>
    vi.stubGlobal('matchMedia', (q: string) => ({
      matches: q.includes('56.2501rem') ? wide : false,
      media: q,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
    }))
  window.history.replaceState(null, '', '/w/1/boards')
  fakeFetch(desks(two))
  query(false)
  const narrow = render(<Shell me={me} onLogout={() => undefined} />)
  await screen.findByTestId('page')
  expect(screen.queryByRole('tablist')).toBeNull()
  narrow.unmount()
  query(true)
  render(<Shell me={me} onLogout={() => undefined} />)
  await screen.findAllByRole('tablist')
})
