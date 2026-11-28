// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { App } from '../App'
import { resetInstallInfo } from '../pwa/install'
import { beginSession, endSession, SessionEnded } from '../session'
import { NotificationsTab } from '../settings/NotificationsTab'
import { Shell } from '../shell/Shell'
import { fakeFetch } from '../test/fakes'
import { NotificationsPage } from './NotificationsPage'
import { disablePush, enablePush, leavePush, PushBusy, sendPushStrings, syncPush } from './push'

const SERVER_KEY = 'AQID'

const me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' as const }],
  can_create_workspace: false,
  is_operator: false,
}

type Sub = ReturnType<typeof fakeSub>

function fakeSub(endpoint = 'https://push.example.net/abc', key: number[] = [1, 2, 3]) {
  return {
    endpoint,
    options: { applicationServerKey: new Uint8Array(key).buffer },
    toJSON: () => ({ endpoint, expirationTime: null, keys: { p256dh: 'device-key', auth: 'device-auth' } }),
    unsubscribe: vi.fn(async () => true),
  }
}

function pushBrowser({
  permission = 'default',
  answer = 'granted',
  subscription = null as Sub | null,
}: {
  permission?: string
  answer?: string
  subscription?: Sub | null
} = {}) {
  const state = { permission, subscription }
  const FakeNotification = {
    get permission() {
      return state.permission
    },
    requestPermission: vi.fn(async () => {
      state.permission = answer
      return answer
    }),
  }
  const subscribe = vi.fn(async (_options?: unknown) => {
    state.subscription = fakeSub()
    return state.subscription
  })
  const postMessage = vi.fn()
  const registration = {
    active: { postMessage },
    pushManager: { getSubscription: vi.fn(async () => state.subscription), subscribe },
  }
  Object.defineProperty(window, 'isSecureContext', { value: true, configurable: true })
  vi.stubGlobal('Notification', FakeNotification)
  vi.stubGlobal('PushManager', class {})
  Object.defineProperty(navigator, 'serviceWorker', {
    value: {
      getRegistration: vi.fn(async () => registration),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    },
    configurable: true,
  })
  return { state, FakeNotification, subscribe, postMessage }
}

function ios(installed: boolean) {
  vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue('Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X)')
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => ({
      matches: installed && query.includes('standalone'),
      addEventListener() {},
      removeEventListener() {},
    })),
  )
}

beforeEach(() => {
  window.history.replaceState(null, '', '/')
  localStorage.clear()
  resetInstallInfo()
  beginSession(1)
})
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  Reflect.deleteProperty(navigator, 'serviceWorker')
})

const pushCalls = (calls: { path: string }[]) => calls.filter((c) => c.path.startsWith('/api/me/push'))

test('a browser that cannot push is told so, keeps the inbox, and nothing is asked of the server', async () => {
  const calls = fakeFetch({})
  render(<NotificationsTab me={1} workspace={1} />)
  await screen.findByText(/can't receive push notifications here/)
  expect(screen.queryByRole('button', { name: /Turn on push/ })).toBeNull()
  expect(screen.getByRole('link', { name: 'Open the inbox' }).getAttribute('href')).toBe('/w/1/notifications')
  expect(pushCalls(calls)).toEqual([])
})

test('on iOS outside the installed app, push is explained as needing the Home Screen app', async () => {
  ios(false)
  const calls = fakeFetch({})
  render(<NotificationsTab me={1} workspace={1} />)
  await screen.findByText(/only in the installed app/)
  expect(screen.queryByRole('button', { name: /Turn on push/ })).toBeNull()
  expect(pushCalls(calls)).toEqual([])
})

test('on iOS in the installed app, push can be turned on', async () => {
  ios(true)
  pushBrowser()
  fakeFetch({})
  render(<NotificationsTab me={1} workspace={1} />)
  await screen.findByRole('button', { name: 'Turn on push on this device' })
})

test('a blocked permission is explained, with no button to ask again and the inbox kept', async () => {
  pushBrowser({ permission: 'denied' })
  const calls = fakeFetch({})
  render(<NotificationsTab me={1} workspace={1} />)
  await screen.findByText(/blocked for this site in your browser/)
  expect(screen.queryByRole('button', { name: /Turn on push/ })).toBeNull()
  expect(screen.getByRole('link', { name: 'Open the inbox' })).toBeTruthy()
  expect(pushCalls(calls)).toEqual([])
})

test('nothing is asked or subscribed until the person turns push on, and then only what the server takes', async () => {
  const browser = pushBrowser()
  const calls = fakeFetch({
    'GET /api/me/push/key': { body: { public_key: SERVER_KEY } },
    'PUT /api/me/push/subscription': { status: 204 },
  })
  render(<NotificationsTab me={1} workspace={1} />)
  const button = await screen.findByRole('button', { name: 'Turn on push on this device' })
  expect(browser.FakeNotification.requestPermission).not.toHaveBeenCalled()
  expect(browser.subscribe).not.toHaveBeenCalled()
  expect(pushCalls(calls)).toEqual([])
  expect(screen.getByText(/Push is off on this device/)).toBeTruthy()

  fireEvent.click(button)
  await screen.findByText('Push is on for this device.')
  expect(browser.FakeNotification.requestPermission).toHaveBeenCalledOnce()
  const options = browser.subscribe.mock.calls[0]?.[0] as unknown as {
    userVisibleOnly: boolean
    applicationServerKey: Uint8Array
  }
  expect(options.userVisibleOnly).toBe(true)
  expect([...options.applicationServerKey]).toEqual([1, 2, 3])
  const put = calls.find((c) => c.method === 'PUT')
  expect(put?.path).toBe('/api/me/push/subscription')
  expect(put?.body).toEqual({
    endpoint: 'https://push.example.net/abc',
    keys: { p256dh: 'device-key', auth: 'device-auth' },
  })
  expect(browser.postMessage).toHaveBeenCalledWith({
    type: 'PUSH_STRINGS',
    strings: {
      card_assigned: 'A card was assigned to you',
      card_due: 'A card of yours is due',
      card_overdue: 'A card of yours is overdue',
      card_comment: 'A card of yours has a new comment',
      card_mention: 'You were mentioned in a comment',
      app_effect_failed: 'An app action needs your attention',
      app_schedule_failed: 'A scheduled app action needs your attention',
      integration_event: 'A payment needs an admin',
      changed: 'Something changed',
    },
  })
  expect(localStorage.getItem('taskto.push')).toBe('1')
  expect(screen.getByText(/never what it is/)).toBeTruthy()
})

test('a person who closes the browser prompt without answering is told push is still off, and nothing is registered', async () => {
  const browser = pushBrowser({ answer: 'default' })
  const calls = fakeFetch({ 'GET /api/me/push/key': { body: { public_key: SERVER_KEY } } })
  render(<NotificationsTab me={1} workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Turn on push on this device' }))
  await screen.findByText(/You didn't answer the browser's question/)
  expect(browser.subscribe).not.toHaveBeenCalled()
  expect(calls.filter((c) => c.method !== 'GET')).toEqual([])
  expect(localStorage.getItem('taskto.push')).toBeNull()
  expect(screen.getByRole('button', { name: 'Turn on push on this device' })).toBeTruthy()
})

test('a person who says no is shown the blocked explanation, and nothing is registered', async () => {
  const browser = pushBrowser({ answer: 'denied' })
  const calls = fakeFetch({ 'GET /api/me/push/key': { body: { public_key: SERVER_KEY } } })
  render(<NotificationsTab me={1} workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Turn on push on this device' }))
  await screen.findByText(/blocked for this site in your browser/)
  expect(browser.subscribe).not.toHaveBeenCalled()
  expect(calls.filter((c) => c.method !== 'GET')).toEqual([])
})

test('when the server refuses the subscription, the browser keeps none and the person sees why', async () => {
  const browser = pushBrowser()
  fakeFetch({
    'GET /api/workspaces/1/notification-email': { body: emailChoices({ available: false }) },
    'GET /api/me/push/key': { body: { public_key: SERVER_KEY } },
    'PUT /api/me/push/subscription': {
      status: 409,
      body: { error: 'too many', code: 'push_too_many_subscriptions', params: { max: 20 } },
    },
  })
  render(<NotificationsTab me={1} workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Turn on push on this device' }))
  expect((await screen.findByRole('alert')).textContent).toBe(
    'You already have push turned on for 20 devices. Turn it off on one of them first.',
  )
  expect(browser.state.subscription?.unsubscribe).toHaveBeenCalled()
  expect(localStorage.getItem('taskto.push')).toBeNull()
})

test('turning push off removes the subscription from the server and the browser', async () => {
  const sub = fakeSub()
  pushBrowser({ permission: 'granted', subscription: sub })
  localStorage.setItem('taskto.push', '1')
  const calls = fakeFetch({ 'DELETE /api/me/push/subscription': { status: 204 } })
  render(<NotificationsTab me={1} workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Turn off push on this device' }))
  await screen.findByRole('button', { name: 'Turn on push on this device' }).catch(() => {})
  await waitFor(() => expect(sub.unsubscribe).toHaveBeenCalled())
  expect(calls.find((c) => c.method === 'DELETE')?.body).toEqual({ endpoint: 'https://push.example.net/abc' })
  expect(localStorage.getItem('taskto.push')).toBeNull()
})

test('a device whose person turned push on is registered again at start; another person never gets it', async () => {
  const sub = fakeSub()
  pushBrowser({ permission: 'granted', subscription: sub })
  const calls = fakeFetch({ 'PUT /api/me/push/subscription': { status: 204 } })

  expect(await syncPush(1)).toBe(false)
  localStorage.setItem('taskto.push', '2')
  expect(await syncPush(1)).toBe(false)
  expect(pushCalls(calls)).toEqual([])
  localStorage.setItem('taskto.push', '1')
  expect(await syncPush(1)).toBe(true)
  expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(1)
})

test('a device that lost its permission is forgotten, not registered', async () => {
  pushBrowser({ permission: 'denied', subscription: fakeSub() })
  localStorage.setItem('taskto.push', '1')
  const calls = fakeFetch({})
  expect(await syncPush(1)).toBe(false)
  expect(pushCalls(calls)).toEqual([])
  expect(localStorage.getItem('taskto.push')).toBeNull()
})

test('signing out removes this device once the server confirmed it, which dropped the session’s subscription itself', async () => {
  const sub = fakeSub()
  pushBrowser({ permission: 'granted', subscription: sub })
  localStorage.setItem('taskto.push', '1')
  window.history.replaceState(null, '', '/w/1/notifications')
  const calls = fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: me },
    'PUT /api/me/push/subscription': { status: 204 },
    'DELETE /api/me/push/subscription': { status: 204 },
    'POST /api/logout': { status: 204 },
    'GET /api/workspaces/1/notifications/count': { body: { unread: 0 } },
    'GET /api/workspaces/1/notifications': { body: { notifications: [], more: false, unread: 0 } },
  })
  render(<App />)
  await screen.findByRole('heading', { name: 'Notifications' })
  fireEvent.click(screen.getByRole('button', { name: /^Account: Owner/ }))
  await act(async () => {
    fireEvent.click(screen.getByRole('menuitem', { name: 'Log out' }))
  })
  await screen.findByRole('heading', { name: 'Log in' })
  const order = calls
    .filter((c) => c.method === 'DELETE' || c.path === '/api/logout')
    .map((c) => `${c.method} ${c.path}`)
  expect(order).toEqual(['POST /api/logout'])
  await waitFor(() => expect(sub.unsubscribe).toHaveBeenCalled())
  expect(localStorage.getItem('taskto.push')).toBeNull()
})

const item = (id: number, over: Record<string, unknown> = {}) => ({
  id,
  workspace_id: 1,
  kind: 'card_assigned',
  params: { title: `Card ${id}` },
  target: { section: 'boards', item: `3:4:${id}` },
  read: false,
  created_at: '2026-03-01T10:00:00.000Z',
  ...over,
})

test('the inbox lists notifications, marks one read when it is opened, and all read together, with push unavailable', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/notifications': {
      body: {
        notifications: [
          item(3),
          item(2, {
            kind: 'app_effect_failed',
            params: { app: 'Inspections' },
            target: { section: 'apps', item: '3-7' },
          }),
          item(1, { kind: 'changed', params: undefined, target: { section: 'home' }, read: true }),
        ],
        more: false,
        unread: 2,
      },
    },
    'POST /api/workspaces/1/notifications/3/read': { status: 204 },
    'POST /api/workspaces/1/notifications/read': { status: 204 },
  })
  render(<NotificationsPage workspace={1} />)
  const list = await screen.findByRole('list', { name: 'Notifications' })
  const links = within(list).getAllByRole('link')
  expect(links.map((a) => a.textContent)).toEqual([
    'Unread: A card was assigned to you: Card 3',
    "Unread: An action in Inspections didn't finish and needs you to retry it",
    'Something changed. Open it to see what you can still see.',
  ])
  expect(links.map((a) => a.getAttribute('href'))).toEqual(['/w/1/boards/3%3A4%3A3', '/w/1/apps/3-7', '/w/1/home'])
  expect(screen.getByRole('link', { name: 'Notification settings' }).getAttribute('href')).toBe(
    '/w/1/settings/notifications',
  )

  fireEvent.click(links[0])
  await waitFor(() => expect(calls.some((c) => c.path === '/api/workspaces/1/notifications/3/read')).toBe(true))
  expect(window.location.pathname).toBe('/w/1/boards/3%3A4%3A3')
  expect((await screen.findAllByText(/Unread:/)).length).toBe(1)

  fireEvent.click(screen.getByRole('button', { name: 'Mark all as read' }))
  await waitFor(() => expect(screen.queryByText(/Unread:/)).toBeNull())
  expect((screen.getByRole('button', { name: 'Mark all as read' }) as HTMLButtonElement).disabled).toBe(true)
})

test("a due and an overdue card say the date in the reader's format, and lead to the card", async () => {
  fakeFetch({
    'GET /api/workspaces/1/notifications': {
      body: {
        notifications: [
          item(2, { kind: 'card_overdue', params: { title: 'Pay rent', due: '2026-03-31' } }),
          item(1, { kind: 'card_due', params: { title: 'Pay rent', due: '2026-03-31' } }),
        ],
        more: false,
        unread: 2,
      },
    },
  })
  render(<NotificationsPage workspace={1} />)
  const list = await screen.findByRole('list', { name: 'Notifications' })
  const links = within(list).getAllByRole('link')
  expect(links.map((a) => a.textContent)).toEqual([
    'Unread: Overdue since Mar 31, 2026: Pay rent',
    'Unread: Due Mar 31, 2026: Pay rent',
  ])
  expect(links.map((a) => a.getAttribute('href'))).toEqual(['/w/1/boards/3%3A4%3A2', '/w/1/boards/3%3A4%3A1'])
})

test('the inbox pages older notifications in, and says when there are none', async () => {
  const calls = fakeFetch({
    'GET /api/workspaces/1/notifications?limit=30': {
      body: { notifications: [item(31), item(30)], more: true, unread: 0 },
    },
    'GET /api/workspaces/1/notifications?limit=30&before=30': {
      body: { notifications: [item(29)], more: false, unread: 0 },
    },
  })
  render(<NotificationsPage workspace={1} />)
  await screen.findByText('A card was assigned to you: Card 30', { exact: false })
  fireEvent.click(screen.getByRole('button', { name: 'Show older' }))
  await screen.findByText('A card was assigned to you: Card 29', { exact: false })
  expect(screen.queryByRole('button', { name: 'Show older' })).toBeNull()
  expect(calls.filter((c) => c.path.includes('before=30'))).toHaveLength(1)

  cleanup()
  fakeFetch({ 'GET /api/workspaces/1/notifications': { body: { notifications: [], more: false, unread: 0 } } })
  render(<NotificationsPage workspace={1} />)
  await screen.findByText('No notifications yet.')
})

test('each notification says where it happened and when: how long ago for the last day, kept current, with the date and time on hover; the date and time after that', async () => {
  vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
  vi.setSystemTime(new Date('2026-03-01T12:00:00.000Z'))
  try {
    fakeFetch({
      'GET /api/workspaces/1/notifications': {
        body: {
          notifications: [
            item(3),
            item(2, {
              kind: 'app_effect_failed',
              params: { app: 'Inspections' },
              target: { section: 'apps' },
              read: true,
            }),
            item(1, { created_at: '2026-01-25T09:30:00.000Z', read: true }),
          ],
          more: false,
          unread: 1,
        },
      },
    })
    render(<NotificationsPage workspace={1} />)
    const list = await screen.findByRole('list', { name: 'Notifications' })
    const [recent, app, old] = within(list).getAllByRole('listitem')
    expect(within(recent).getByText('Boards ·')).toBeTruthy()
    expect(within(app).getByText('Apps ·')).toBeTruthy()
    const when = recent.querySelector('time') as HTMLTimeElement
    expect(when.textContent).toBe('2 hours ago')
    expect(when.dateTime).toBe('2026-03-01T10:00:00.000Z')
    expect(when.tabIndex).toBe(0)
    const described = document.getElementById(when.getAttribute('aria-describedby') ?? '')
    expect(described?.textContent).toMatch(/Mar 1, 2026/)
    const oldWhen = old.querySelector('time') as HTMLTimeElement
    expect(oldWhen.dateTime).toBe('2026-01-25T09:30:00.000Z')
    expect(oldWhen.textContent).toMatch(/Jan 25, 2026/)
    expect(oldWhen.textContent).not.toMatch(/ago|last/)
    act(() => {
      vi.setSystemTime(new Date('2026-03-01T13:00:00.000Z'))
      vi.advanceTimersByTime(30_000)
    })
    expect(when.textContent).toBe('3 hours ago')
  } finally {
    vi.useRealTimers()
  }
})

test('an unread notification is marked for the eye and the ear, a read one is not', async () => {
  fakeFetch({
    'GET /api/workspaces/1/notifications': {
      body: { notifications: [item(2), item(1, { read: true })], more: false, unread: 1 },
    },
  })
  render(<NotificationsPage workspace={1} />)
  const list = await screen.findByRole('list', { name: 'Notifications' })
  const [unread, read] = within(list).getAllByRole('listitem')
  expect(within(unread).getByRole('link').textContent).toMatch(/^Unread: /)
  expect(within(read).getByRole('link').textContent).not.toMatch(/Unread/)
  expect(unread.querySelector('[data-unread]')).toBeTruthy()
  expect(read.querySelector('[data-unread]')).toBeNull()
})

test('a notification that may not say its subject does not say where it happened either', async () => {
  fakeFetch({
    'GET /api/workspaces/1/notifications': {
      body: {
        notifications: [item(1, { kind: 'changed', params: undefined, target: { section: 'home' } })],
        more: false,
        unread: 1,
      },
    },
  })
  render(<NotificationsPage workspace={1} />)
  const list = await screen.findByRole('list', { name: 'Notifications' })
  const row = within(list).getByRole('listitem')
  expect(within(row).queryByText(/Home/)).toBeNull()
  expect(row.querySelector('time')).toBeTruthy()
})

test('an empty inbox says what will show there', async () => {
  fakeFetch({ 'GET /api/workspaces/1/notifications': { body: { notifications: [], more: false, unread: 0 } } })
  render(<NotificationsPage workspace={1} />)
  await screen.findByText('No notifications yet.')
  expect(screen.getByText(/When a card is assigned to you/)).toBeTruthy()
  expect((screen.getByRole('button', { name: 'Mark all as read' }) as HTMLButtonElement).disabled).toBe(true)
})

test("the sidebar's inbox has the unread count, which opens it", async () => {
  window.history.replaceState(null, '', '/w/1/home')
  fakeFetch({
    'GET /api/workspaces/1/notifications/count': { body: { unread: 120 } },
    'GET /api/workspaces/1/notifications': { body: { notifications: [], more: false, unread: 120 } },
    'GET /api/workspaces/1/home': { body: { items: [] } },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  const bell = await screen.findByRole('link', { name: 'Notifications, 120 unread' })
  expect(bell.getAttribute('href')).toBe('/w/1/notifications')
  expect(bell.textContent).toBe('Notifications99+')
  fireEvent.click(bell)
  await screen.findByRole('heading', { name: 'Notifications' })
})

test('a server without an inbox, or one that fails, leaves the bell quiet and the app usable', async () => {
  window.history.replaceState(null, '', '/w/1/knowledge')
  fakeFetch({ 'GET /api/workspaces/1/spaces': { body: { spaces: [] } } })
  render(<Shell me={me} onLogout={() => {}} />)
  const bell = await screen.findByRole('link', { name: 'Notifications' })
  expect(bell.textContent).toBe('Notifications')
})

test('the address a push opens reads the notification as the person, marks it read and goes where it leads', async () => {
  window.history.replaceState(null, '', '/notification/5')
  const calls = fakeFetch({
    'GET /api/me/notifications/5': { body: item(5, { workspace_id: 1 }) },
    'POST /api/workspaces/1/notifications/5/read': { status: 204 },
    'GET /api/workspaces/1/spaces': { body: { spaces: [] } },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/boards/3%3A4%3A5'))
  expect(calls.some((c) => c.method === 'POST' && c.path === '/api/workspaces/1/notifications/5/read')).toBe(true)
})

test("a notification that is not the signed-in person's, or is gone, says so and offers the inbox", async () => {
  window.history.replaceState(null, '', '/notification/6')
  fakeFetch({ 'GET /api/me/notifications/6': { status: 404, body: { error: 'x', code: 'notification_not_found' } } })
  render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByText("This notification isn't available any more.")
  expect(screen.getByRole('link', { name: 'Open your notifications' }).getAttribute('href')).toBe('/w/1/notifications')
  expect(window.location.pathname).toBe('/notification/6')
})

test('the open app follows the service worker to the notification a click named, and to nothing else', async () => {
  const browser = pushBrowser()
  void browser
  window.history.replaceState(null, '', '/w/1/home')
  fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: me },
    'GET /api/workspaces/1/notifications/count': { body: { unread: 0 } },
    'GET /api/workspaces/1/home': { body: { items: [] } },
    'GET /api/me/notifications/8': { body: item(8) },
    'POST /api/workspaces/1/notifications/8/read': { status: 204 },
  })
  render(<App />)
  await screen.findByRole('link', { name: 'Notifications' })
  const container = navigator.serviceWorker as unknown as { addEventListener: ReturnType<typeof vi.fn> }
  const handlers = container.addEventListener.mock.calls
    .filter((c) => c[0] === 'message')
    .map((c) => c[1] as (e: MessageEvent) => void)
  expect(handlers.length).toBeGreaterThan(0)
  const send = (data: unknown) =>
    act(() => {
      for (const h of handlers) h({ data } as MessageEvent)
    })
  send({ type: 'OPEN', path: '/api/me' })
  send({ type: 'OPEN', path: 'https://evil.example/notification/8' })
  send({ type: 'OPEN', path: '/w/1/settings' })
  expect(window.location.pathname).toBe('/w/1/home')
  send({ type: 'OPEN', path: '/notification/8' })
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/boards/3%3A4%3A8'))
})

test('a click on a push while the login is showing is kept: signing in opens the notification', async () => {
  pushBrowser()
  window.history.replaceState(null, '', '/w/1/home')
  let session = false
  const calls = fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': () => (session ? { body: me } : { status: 401, body: { error: 'log in', code: 'not_logged_in' } }),
    'POST /api/login': () => {
      session = true
      return { body: me }
    },
    'GET /api/workspaces/1/notifications/count': { body: { unread: 0 } },
    'GET /api/workspaces/1/spaces': { body: { spaces: [] } },
    'GET /api/me/notifications/8': { body: item(8) },
    'POST /api/workspaces/1/notifications/8/read': { status: 204 },
  })
  render(<App />)
  await screen.findByRole('heading', { name: 'Log in' })
  const container = navigator.serviceWorker as unknown as { addEventListener: ReturnType<typeof vi.fn> }
  const handlers = container.addEventListener.mock.calls
    .filter((c) => c[0] === 'message')
    .map((c) => c[1] as (e: MessageEvent) => void)
  act(() => {
    for (const h of handlers) h({ data: { type: 'OPEN', path: '/notification/8' } } as MessageEvent)
  })
  expect(window.location.pathname).toBe('/notification/8')
  expect(screen.getByRole('heading', { name: 'Log in' })).toBeTruthy()
  expect(calls.some((c) => c.path === '/api/me/notifications/8')).toBe(false)

  fireEvent.change(screen.getByLabelText('Email'), { target: { value: 'owner@example.com' } })
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'right password!' } })
  fireEvent.click(screen.getByRole('button', { name: 'Log in' }))
  await waitFor(() => expect(window.location.pathname).toBe('/w/1/boards/3%3A4%3A8'))
  expect(calls.some((c) => c.method === 'POST' && c.path === '/api/workspaces/1/notifications/8/read')).toBe(true)
})

test('a screen that holds a one-time token in its address is not replaced by a click on a push', async () => {
  pushBrowser()
  window.history.replaceState(null, '', '/reset-password#token-value')
  fakeFetch({ 'GET /api/setup': { body: { needed: false } } })
  render(<App />)
  await screen.findByRole('heading', { name: /password/i })
  await act(async () => {})
  const container = navigator.serviceWorker as unknown as {
    addEventListener: ReturnType<typeof vi.fn>
    removeEventListener: ReturnType<typeof vi.fn>
  }
  const removed = container.removeEventListener.mock.calls.map((c) => c[1])
  const listening = container.addEventListener.mock.calls
    .filter((c) => c[0] === 'message' && !removed.includes(c[1]))
    .map((c) => c[1] as (e: MessageEvent) => void)
  act(() => {
    for (const h of listening) h({ data: { type: 'OPEN', path: '/notification/8' } } as MessageEvent)
  })
  expect(window.location.pathname).toBe('/reset-password')
  expect(window.location.hash).toBe('#token-value')
})

test('when the server cannot be told, push stays on, as it is, with the reason and a way to try again', async () => {
  const sub = fakeSub()
  pushBrowser({ permission: 'granted', subscription: sub })
  localStorage.setItem('taskto.push', '1')
  let down = true
  const calls = fakeFetch({
    'DELETE /api/me/push/subscription': () => (down ? { status: 500, body: {} } : { status: 204 }),
  })
  render(<NotificationsTab me={1} workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Turn off push on this device' }))
  await screen.findByRole('alert')
  expect(sub.unsubscribe).not.toHaveBeenCalled()
  expect(localStorage.getItem('taskto.push')).toBe('1')
  expect(screen.getByText('Push is on for this device.')).toBeTruthy()

  down = false
  fireEvent.click(screen.getByRole('button', { name: 'Turn off push on this device' }))
  await screen.findByText('Push is off on this device.')
  expect(sub.unsubscribe).toHaveBeenCalled()
  expect(localStorage.getItem('taskto.push')).toBeNull()
  expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(2)
})

test("when the browser won't end its subscription, push stays on and the person is told", async () => {
  const sub = fakeSub()
  sub.unsubscribe.mockResolvedValueOnce(false)
  pushBrowser({ permission: 'granted', subscription: sub })
  localStorage.setItem('taskto.push', '1')
  fakeFetch({
    'GET /api/workspaces/1/notification-email': { body: emailChoices({ available: false }) },
    'DELETE /api/me/push/subscription': { status: 204 },
  })
  render(<NotificationsTab me={1} workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Turn off push on this device' }))
  expect((await screen.findByRole('alert')).textContent).toMatch(/wouldn't end its push subscription/)
  expect(localStorage.getItem('taskto.push')).toBe('1')
  expect(screen.getByText('Push is on for this device.')).toBeTruthy()
})

test('opening the app signed in with no workspace registers nothing', async () => {
  pushBrowser({ permission: 'granted', subscription: fakeSub() })
  localStorage.setItem('taskto.push', '1')
  window.history.replaceState(null, '', '/')
  const calls = fakeFetch({
    'GET /api/setup': { body: { needed: false } },
    'GET /api/me': { body: { ...me, workspaces: [] } },
    'PUT /api/me/push/subscription': { status: 204 },
  })
  render(<App />)
  await waitFor(() => expect(calls.some((c) => c.path === '/api/me')).toBe(true))
  await act(async () => {})
  expect(pushCalls(calls)).toEqual([])
})

const emailChoices = (over: Record<string, unknown> = {}) => ({
  available: true,
  address: { email: 'owner@example.com', state: 'verified' },
  kinds: {
    card_assigned: 'off',
    card_comment: 'off',
    card_mention: 'off',
    card_due: 'off',
    card_overdue: 'off',
    app_effect_failed: 'off',
    app_schedule_failed: 'off',
  },
  digest: { hour: 8, timezone: 'UTC', chosen: false },
  ...over,
})

test('email is chosen per kind, as it happens or in a daily summary, and is off until then', async () => {
  pushBrowser({ permission: 'denied' })
  let saved = emailChoices()
  const calls = fakeFetch({
    'GET /api/workspaces/1/notification-email': () => ({ body: saved }),
    'PUT /api/workspaces/1/notification-email': (call) => {
      const body = call.body as { kinds?: Record<string, string>; digest?: { hour: number; timezone: string } }
      saved = {
        ...saved,
        kinds: { ...saved.kinds, ...body.kinds },
        digest: body.digest ? { ...body.digest, chosen: true } : saved.digest,
      } as typeof saved
      return { body: saved }
    },
  })
  render(<NotificationsTab me={1} workspace={1} />)
  const assigned = (await screen.findByLabelText('A card was assigned to you')) as HTMLSelectElement
  expect(assigned.value).toBe('off')
  expect(screen.queryByLabelText('Send the daily summary at')).toBeNull()

  fireEvent.change(assigned, { target: { value: 'instant' } })
  await waitFor(() => expect(assigned.value).toBe('instant'))
  fireEvent.change(screen.getByLabelText('An app action needs your attention'), { target: { value: 'digest' } })
  await screen.findByLabelText('Send the daily summary at')
  fireEvent.change(screen.getByLabelText('Send the daily summary at'), { target: { value: '18' } })
  await waitFor(() =>
    expect((screen.getByLabelText('Send the daily summary at') as HTMLSelectElement).value).toBe('18'),
  )

  const puts = calls.filter((c) => c.method === 'PUT').map((c) => c.body as Record<string, unknown>)
  expect(puts[0]).toEqual({ kinds: { card_assigned: 'instant' } })
  expect(puts[1]).toEqual({ kinds: { app_effect_failed: 'digest' } })
  expect(puts[2]).toEqual({ digest: { hour: 18, timezone: 'UTC' } })
})

test('every kind that can be mailed has a choice of its own, off until chosen', async () => {
  pushBrowser({ permission: 'denied' })
  let saved = emailChoices()
  const calls = fakeFetch({
    'GET /api/workspaces/1/notification-email': () => ({ body: saved }),
    'PUT /api/workspaces/1/notification-email': (call) => {
      const body = call.body as { kinds?: Record<string, string> }
      saved = { ...saved, kinds: { ...saved.kinds, ...body.kinds } } as typeof saved
      return { body: saved }
    },
  })
  render(<NotificationsTab me={1} workspace={1} />)
  const labels: Record<string, string> = {
    card_assigned: 'A card was assigned to you',
    card_comment: 'A card of yours has a new comment',
    card_mention: 'You were mentioned in a comment',
    card_due: 'A card of yours is due',
    card_overdue: 'A card of yours is overdue',
    app_effect_failed: 'An app action needs your attention',
    app_schedule_failed: 'A scheduled app action needs your attention',
  }
  for (const [kind, label] of Object.entries(labels)) {
    const select = (await screen.findByLabelText(label)) as HTMLSelectElement
    expect(select.value).toBe('off')
    fireEvent.change(select, { target: { value: 'instant' } })
    await waitFor(() => expect(select.value).toBe('instant'))
    expect(calls.filter((c) => c.method === 'PUT').pop()?.body).toEqual({ kinds: { [kind]: 'instant' } })
  }
})

test('where the server can not send mail, email says so and offers nothing', async () => {
  pushBrowser({ permission: 'denied' })
  fakeFetch({ 'GET /api/workspaces/1/notification-email': { body: emailChoices({ available: false }) } })
  render(<NotificationsTab me={1} workspace={1} />)
  await screen.findByText(/Email isn't set up on this server/)
  expect(screen.queryByLabelText('A card was assigned to you')).toBeNull()
})

test('email that is on can be turned off where the server can no longer send mail', async () => {
  pushBrowser({ permission: 'denied' })
  let saved = emailChoices({ available: false, kinds: { ...emailChoices().kinds, card_assigned: 'instant' } })
  const calls = fakeFetch({
    'GET /api/workspaces/1/notification-email': () => ({ body: saved }),
    'PUT /api/workspaces/1/notification-email': (call) => {
      saved = { ...saved, kinds: { ...saved.kinds, ...(call.body as { kinds: object }).kinds } } as typeof saved
      return { body: saved }
    },
  })
  render(<NotificationsTab me={1} workspace={1} />)
  await screen.findByText(/You can still turn email off/)
  const assigned = (await screen.findByLabelText('A card was assigned to you')) as HTMLSelectElement
  expect(assigned.value).toBe('instant')
  const options = within(assigned).getAllByRole('option') as HTMLOptionElement[]
  expect(options.filter((o) => o.disabled).map((o) => o.value)).toEqual(['instant', 'digest'])
  fireEvent.change(assigned, { target: { value: 'off' } })
  await screen.findByText(/so notifications can't be sent by email/)
  expect(screen.queryByLabelText('A card was assigned to you')).toBeNull()
  expect(calls.filter((c) => c.method === 'PUT').map((c) => c.body)).toEqual([{ kinds: { card_assigned: 'off' } }])
})

test('the panel says whether the address is confirmed and sends the link', async () => {
  pushBrowser({ permission: 'denied' })
  let saved = emailChoices({
    address: { email: 'owner@example.com', state: 'unverified' },
    kinds: { ...emailChoices().kinds, card_assigned: 'instant' },
  })
  const calls = fakeFetch({
    'GET /api/workspaces/1/notification-email': () => ({ body: saved }),
    'POST /api/workspaces/1/notification-email/verification': () => {
      saved = { ...saved, address: { email: 'owner@example.com', state: 'pending' } }
      return { status: 204 }
    },
  })
  render(<NotificationsTab me={1} workspace={1} />)
  await screen.findByText(/This address isn't confirmed/)
  expect(screen.getByText(/Notifications go to owner@example.com/)).toBeTruthy()
  fireEvent.click(screen.getByRole('button', { name: 'Send the confirmation link' }))
  await screen.findByText(/We sent a link to this address/)
  await screen.findByText(/A link was sent/)
  expect(calls.some((c) => c.method === 'POST' && c.path.endsWith('/notification-email/verification'))).toBe(true)
  expect(screen.getByRole('button', { name: 'Send the link again' })).toBeTruthy()
})

test('a confirmation link confirms the signed-in address once and clears the token from the address bar', async () => {
  window.history.replaceState(null, '', '/confirm-notification-email#abc_DEF-123')
  const calls = fakeFetch({
    'GET /api/me': { body: me },
    'POST /api/me/notification-email/confirm': { status: 204 },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByText(/Your address is confirmed/)
  expect(calls.filter((c) => c.method === 'POST').map((c) => c.body)).toEqual([{ token: 'abc_DEF-123' }])
  expect(window.location.hash).toBe('')
  expect(screen.getByRole('link', { name: 'Open notification settings' }).getAttribute('href')).toBe(
    '/w/1/settings/notifications',
  )
})

test('a confirmation link that does not work says why', async () => {
  window.history.replaceState(null, '', '/confirm-notification-email#used')
  fakeFetch({
    'POST /api/me/notification-email/confirm': {
      status: 400,
      body: { error: 'x', code: 'notification_email_verification_invalid' },
    },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  await screen.findByText(/That confirmation link doesn't work/)
})

test("a time zone the person picked for the summary is not replaced by their profile's when it is turned on again", async () => {
  pushBrowser({ permission: 'denied' })
  let saved = emailChoices({ digest: { hour: 8, timezone: 'America/New_York', chosen: false } })
  const calls = fakeFetch({
    'GET /api/workspaces/1/notification-email': () => ({ body: saved }),
    'PUT /api/workspaces/1/notification-email': (call) => {
      const body = call.body as { kinds?: Record<string, string>; digest?: { hour: number; timezone: string } }
      saved = {
        ...saved,
        kinds: { ...saved.kinds, ...body.kinds },
        digest: body.digest ? { ...body.digest, chosen: true } : saved.digest,
      } as typeof saved
      return { body: saved }
    },
  })
  const first = render(<NotificationsTab me={1} workspace={1} />)
  let assigned = (await screen.findByLabelText('A card was assigned to you')) as HTMLSelectElement
  fireEvent.change(assigned, { target: { value: 'digest' } })
  const zone = (await screen.findByLabelText('Time zone')) as HTMLSelectElement
  await waitFor(() => expect(zone.value).toBe('America/New_York'))
  fireEvent.change(zone, { target: { value: 'UTC' } })
  await waitFor(() => expect(saved.digest.timezone).toBe('UTC'))
  fireEvent.change(assigned, { target: { value: 'off' } })
  await waitFor(() => expect(saved.kinds.card_assigned).toBe('off'))
  first.unmount()
  render(<NotificationsTab me={1} workspace={1} />)
  assigned = (await screen.findByLabelText('A card was assigned to you')) as HTMLSelectElement
  await waitFor(() => expect(assigned.value).toBe('off'))
  fireEvent.change(assigned, { target: { value: 'digest' } })
  await waitFor(() => expect(saved.kinds.card_assigned).toBe('digest'))
  const puts = calls.filter((c) => c.method === 'PUT').map((c) => c.body as { digest?: unknown })
  expect(puts[puts.length - 1]).toEqual({ kinds: { card_assigned: 'digest' } })
  expect(saved.digest.timezone).toBe('UTC')
})

test("a new time zone is shown by the summary, which follows it, and not sent back as the summary's own", async () => {
  pushBrowser({ permission: 'denied' })
  let profile = 'Europe/Lisbon'
  const calls = fakeFetch({
    'GET /api/workspaces/1/notification-email': () => ({
      body: emailChoices({
        kinds: { ...emailChoices().kinds, card_assigned: 'digest' },
        digest: { hour: 8, timezone: profile, chosen: false },
      }),
    }),
    'PATCH /api/me': (call) => {
      profile = (call.body as { time_zone: string }).time_zone
      return { body: { ...me, user: { ...me.user, time_zone: profile } } }
    },
    'PUT /api/workspaces/1/notification-email': (call) => ({
      body: emailChoices({ digest: { ...(call.body as { digest: object }).digest, chosen: true } }),
    }),
  })
  render(<NotificationsTab me={1} workspace={1} timeZone="Europe/Lisbon" />)
  const summaryZone = (await screen.findByLabelText('Time zone')) as HTMLSelectElement
  expect(summaryZone.value).toBe('Europe/Lisbon')

  fireEvent.change(screen.getByLabelText('Your time zone'), { target: { value: 'Asia/Tokyo' } })
  await waitFor(() => expect((screen.getByLabelText('Time zone') as HTMLSelectElement).value).toBe('Asia/Tokyo'))
  fireEvent.change(screen.getByLabelText('Send the daily summary at'), { target: { value: '18' } })
  await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true))
  expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ digest: { hour: 18, timezone: 'Asia/Tokyo' } })
})

test('turning push on, when the session ends while the server key is fetched, subscribes nobody and remembers nobody', async () => {
  const browser = pushBrowser({ permission: 'granted' })
  let key: () => void = () => {}
  const calls = fakeFetch({
    'GET /api/me/push/key': () =>
      new Promise((done) => {
        key = () => done({ body: { public_key: SERVER_KEY } })
      }),
    'PUT /api/me/push/subscription': { status: 204 },
  })
  beginSession(1)
  await expect(
    (async () => {
      const on = enablePush(1)
      await vi.waitFor(() => expect(calls.some((c) => c.path === '/api/me/push/key')).toBe(true))
      endSession()
      beginSession(2)
      key()
      await on
    })(),
  ).rejects.toBeInstanceOf(SessionEnded)
  expect(browser.subscribe).not.toHaveBeenCalled()
  expect(calls.some((c) => c.method === 'PUT')).toBe(false)
  expect(localStorage.getItem('taskto.push')).toBeNull()
})

test('a subscription made just as the session ended is undone, and nothing is remembered', async () => {
  pushBrowser({ permission: 'granted' })
  let put: () => void = () => {}
  const calls = fakeFetch({
    'GET /api/me/push/key': { body: { public_key: SERVER_KEY } },
    'PUT /api/me/push/subscription': () =>
      new Promise((done) => {
        put = () => done({ status: 204 })
      }),
  })
  beginSession(1)
  const on = enablePush(1).catch((e) => e)
  await vi.waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true))
  expect(calls.find((c) => c.method === 'PUT')?.headers).toMatchObject({ 'x-taskto-expected-user': '1' })
  endSession()
  put()
  expect(await on).toBeInstanceOf(SessionEnded)
  const sub = (await (await navigator.serviceWorker.getRegistration())?.pushManager.getSubscription()) as unknown as Sub
  expect(sub.unsubscribe).toHaveBeenCalled()
  expect(localStorage.getItem('taskto.push')).toBeNull()
})

test('turning push off names the account; signing out asks the server nothing (it dropped the session’s subscription)', async () => {
  const sub = fakeSub()
  pushBrowser({ permission: 'granted', subscription: sub })
  const calls = fakeFetch({ 'DELETE /api/me/push/subscription': { status: 204 } })
  beginSession(1)
  await disablePush()
  expect(calls.filter((c) => c.method === 'DELETE').map((c) => c.headers['x-taskto-expected-user'])).toEqual(['1'])
  endSession()
  localStorage.setItem('taskto.push', '1')
  await leavePush(1)
  expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(1)
  expect(sub.unsubscribe).toHaveBeenCalledTimes(2)
  expect(localStorage.getItem('taskto.push')).toBeNull()
})

test('registering again at start stops if the session ended meanwhile: the device is the next person’s to decide', async () => {
  pushBrowser({ permission: 'denied', subscription: fakeSub() })
  localStorage.setItem('taskto.push', '1')
  const calls = fakeFetch({ 'PUT /api/me/push/subscription': { status: 204 } })
  let registered: () => void = () => {}
  const reg = await navigator.serviceWorker.getRegistration()
  Object.defineProperty(navigator, 'serviceWorker', {
    value: {
      getRegistration: () =>
        new Promise((done) => {
          registered = () => done(reg)
        }),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    },
    configurable: true,
  })
  beginSession(1)
  const synced = syncPush(1)
  endSession()
  beginSession(2)
  localStorage.setItem('taskto.push', '2')
  registered()
  expect(await synced).toBe(false)
  expect(localStorage.getItem('taskto.push')).toBe('2')
  expect(pushCalls(calls)).toEqual([])
})

test('the words for a push reach the worker only in the session that sent them', async () => {
  const browser = pushBrowser({ permission: 'granted' })
  beginSession(1)
  const sending = sendPushStrings({ changed: 'Something changed' })
  endSession()
  await sending
  expect(browser.postMessage).not.toHaveBeenCalled()
})

test('a tab whose push turned on as the session ended says nothing of it', async () => {
  pushBrowser({ permission: 'granted' })
  let key: () => void = () => {}
  const calls = fakeFetch({
    'GET /api/me/push/key': () =>
      new Promise((done) => {
        key = () => done({ body: { public_key: SERVER_KEY } })
      }),
  })
  beginSession(1)
  render(<NotificationsTab me={1} workspace={1} />)
  fireEvent.click(await screen.findByRole('button', { name: 'Turn on push on this device' }))
  await waitFor(() => expect(calls.some((c) => c.path === '/api/me/push/key')).toBe(true))
  endSession()
  await act(async () => key())
  await new Promise((r) => setTimeout(r, 20))
  expect(screen.queryByText(/session ended/)).toBeNull()
})

test("a sign-out's push removal that finds the subscription after someone else turned push on leaves it alone", async () => {
  const sub = fakeSub()
  pushBrowser({ permission: 'granted', subscription: sub })
  localStorage.setItem('taskto.push', '1')
  let found: () => void = () => {}
  const reg = await navigator.serviceWorker.getRegistration()
  Object.defineProperty(navigator, 'serviceWorker', {
    value: {
      getRegistration: () =>
        new Promise((done) => {
          found = () => done(reg)
        }),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    },
    configurable: true,
  })
  vi.useFakeTimers({ toFake: ['setTimeout'] })
  const leaving = leavePush(1)
  await vi.advanceTimersByTimeAsync(3000)
  await leaving
  vi.useRealTimers()
  beginSession(2)
  localStorage.setItem('taskto.push', '2')
  found()
  await new Promise((r) => setTimeout(r, 20))
  expect(sub.unsubscribe).not.toHaveBeenCalled()
  expect(localStorage.getItem('taskto.push')).toBe('2')
})

test("turning push on, answered after its person signed out and someone else turned push on, leaves the other's subscription alone", async () => {
  pushBrowser({ permission: 'granted' })
  let put: () => void = () => {}
  const calls = fakeFetch({
    'GET /api/me/push/key': { body: { public_key: SERVER_KEY } },
    'PUT /api/me/push/subscription': () =>
      new Promise((done) => {
        put = () => done({ status: 204 })
      }),
  })
  beginSession(1)
  const on = enablePush(1).catch((e) => e)
  await vi.waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true))
  const sub = (await (await navigator.serviceWorker.getRegistration())?.pushManager.getSubscription()) as unknown as Sub
  endSession()
  beginSession(2)
  localStorage.setItem('taskto.push', '2')
  put()
  expect(await on).toBeInstanceOf(SessionEnded)
  expect(sub.unsubscribe).not.toHaveBeenCalled()
  expect(localStorage.getItem('taskto.push')).toBe('2')
})

test("an older tab's sign-out never ends the subscription another tab is registering for the next account", async () => {
  const sub = fakeSub('https://push.example.net/abc', [1, 2, 3])
  const browser = pushBrowser({ permission: 'granted', subscription: sub })
  sub.unsubscribe.mockImplementation(async () => {
    browser.state.subscription = null
    return true
  })
  localStorage.setItem('taskto.push', '1')
  let put: () => void = () => {}
  const calls = fakeFetch({
    'GET /api/me/push/key': { body: { public_key: SERVER_KEY } },
    'PUT /api/me/push/subscription': () =>
      new Promise((done) => {
        put = () => done({ status: 204 })
      }),
  })
  const reg = await navigator.serviceWorker.getRegistration()
  let found: () => void = () => {}
  let first = true
  Object.defineProperty(navigator, 'serviceWorker', {
    value: {
      getRegistration: () => {
        if (!first) return Promise.resolve(reg)
        first = false
        return new Promise((done) => {
          found = () => done(reg)
        })
      },
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    },
    configurable: true,
  })
  const leaving = leavePush(1)
  const turningOn = enablePush(2).catch((e) => e)
  await new Promise((r) => setTimeout(r, 20))
  found()
  await new Promise((r) => setTimeout(r, 20))
  put()
  await leaving
  await turningOn
  await new Promise((r) => setTimeout(r, 20))
  const registered = calls.filter((c) => c.method === 'PUT')
  const current = (await (
    await navigator.serviceWorker.getRegistration()
  )?.pushManager.getSubscription()) as unknown as Sub | null
  if (registered.length > 0) {
    expect(current).not.toBeNull()
    expect(current?.unsubscribe).not.toHaveBeenCalled()
    expect(localStorage.getItem('taskto.push')).toBe('2')
  }
})

test('where the browser has Web Locks, every change to its push subscription takes the same lock', async () => {
  pushBrowser({ permission: 'granted', subscription: fakeSub() })
  fakeFetch({
    'GET /api/me/push/key': { body: { public_key: SERVER_KEY } },
    'PUT /api/me/push/subscription': { status: 204 },
    'DELETE /api/me/push/subscription': { status: 204 },
  })
  const names: string[] = []
  Object.defineProperty(navigator, 'locks', {
    configurable: true,
    value: {
      request: (name: string, _options: unknown, work: () => Promise<unknown>) => {
        names.push(name)
        return work()
      },
    },
  })
  try {
    await enablePush(1)
    await disablePush()
    localStorage.setItem('taskto.push', '1')
    await leavePush(1)
  } finally {
    Reflect.deleteProperty(navigator, 'locks')
  }
  expect(names).toEqual(['taskto-push-subscription', 'taskto-push-subscription', 'taskto-push-subscription'])
})

test('turning push on claims this browser’s subscription for its account before it reuses it', async () => {
  pushBrowser({ permission: 'granted', subscription: fakeSub() })
  localStorage.setItem('taskto.push', '1')
  let put: () => void = () => {}
  fakeFetch({
    'GET /api/me/push/key': { body: { public_key: SERVER_KEY } },
    'PUT /api/me/push/subscription': () =>
      new Promise((done) => {
        put = () => done({ status: 204 })
      }),
  })
  beginSession(2)
  const on = enablePush(2)
  await vi.waitFor(() => expect(localStorage.getItem('taskto.push')).toBe('2'))
  put()
  await on
  expect(localStorage.getItem('taskto.push')).toBe('2')
})

function heldUnsubscribe() {
  const sub = fakeSub()
  let end: (ended: boolean) => void = () => {}
  sub.unsubscribe = vi.fn(
    () =>
      new Promise<boolean>((done) => {
        end = done
      }),
  )
  return { sub, end: (ended = true) => end(ended) }
}

test('turning push on is for the session of the click: a turn that comes after the next person signed in does nothing', async () => {
  const held = heldUnsubscribe()
  const browser = pushBrowser({ permission: 'granted', subscription: held.sub })
  const calls = fakeFetch({
    'GET /api/me/push/key': { body: { public_key: SERVER_KEY } },
    'PUT /api/me/push/subscription': { status: 204 },
  })
  localStorage.setItem('taskto.push', '9')
  const leaving = leavePush(9)
  await vi.waitFor(() => expect(held.sub.unsubscribe).toHaveBeenCalled())
  const on = enablePush(1)
  endSession()
  beginSession(2)
  held.end()
  await expect(on).rejects.toBeInstanceOf(SessionEnded)
  await leaving
  expect(calls.some((c) => c.method === 'PUT')).toBe(false)
  expect(browser.subscribe).not.toHaveBeenCalled()
  expect(localStorage.getItem('taskto.push')).toBeNull()
})

test('without Web Locks, a subscription another tab is still ending is never reused: turning on says to try again', async () => {
  const browser = pushBrowser({ permission: 'granted', subscription: fakeSub() })
  const calls = fakeFetch({
    'GET /api/me/push/key': { body: { public_key: SERVER_KEY } },
    'PUT /api/me/push/subscription': { status: 204 },
  })
  localStorage.setItem('taskto.push-unsubscribing:other-tab', String(Date.now()))
  await expect(enablePush(1)).rejects.toBeInstanceOf(PushBusy)
  localStorage.setItem('taskto.push', '1')
  expect(await syncPush(1)).toBe(false)
  expect(calls.some((c) => c.method === 'PUT')).toBe(false)
  expect(browser.subscribe).not.toHaveBeenCalled()

  localStorage.removeItem('taskto.push-unsubscribing:other-tab')
  expect(await syncPush(1)).toBe(true)
})

test('waiting for a turn to change push is bounded, and the turn held by a change the browser is still making is never given up', async () => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
  try {
    const held = heldUnsubscribe()
    pushBrowser({ permission: 'granted', subscription: held.sub })
    const calls = fakeFetch({
      'GET /api/me/push/key': { body: { public_key: SERVER_KEY } },
      'PUT /api/me/push/subscription': { status: 204 },
    })
    localStorage.setItem('taskto.push', '9')
    const leaving = leavePush(9)
    await vi.waitFor(() => expect(held.sub.unsubscribe).toHaveBeenCalled())
    const on = enablePush(1)
    const settled = expect(on).rejects.toBeInstanceOf(PushBusy)
    await vi.advanceTimersByTimeAsync(10_000)
    await settled
    const off = disablePush().catch((e) => e)
    await vi.advanceTimersByTimeAsync(1_000)
    expect(calls.some((c) => c.method === 'DELETE' || c.method === 'PUT')).toBe(false)
    held.end()
    await leaving
    await off
  } finally {
    vi.useRealTimers()
  }
})

test('turning push off never ends a subscription another account claimed while the server answered', async () => {
  const sub = fakeSub()
  pushBrowser({ permission: 'granted', subscription: sub })
  localStorage.setItem('taskto.push', '1')
  let answer: () => void = () => {}
  fakeFetch({
    'DELETE /api/me/push/subscription': () =>
      new Promise((done) => {
        answer = () => done({ status: 204 })
      }),
  })
  const off = disablePush()
  await vi.waitFor(() => expect(localStorage.getItem('taskto.push')).toBe('1'))
  await new Promise((r) => setTimeout(r, 0))
  localStorage.setItem('taskto.push', '2')
  answer()
  await off
  expect(sub.unsubscribe).not.toHaveBeenCalled()
  expect(localStorage.getItem('taskto.push')).toBe('2')
})

test("a sign-out's turn after another account's failed turning on leaves no opt-in of the departing account", async () => {
  pushBrowser({ permission: 'granted', subscription: fakeSub() })
  localStorage.setItem('taskto.push', '1')
  let refuse: () => void = () => {}
  fakeFetch({
    'GET /api/me/push/key': { body: { public_key: SERVER_KEY } },
    'PUT /api/me/push/subscription': () =>
      new Promise((done) => {
        refuse = () => done({ status: 500, body: { error: 'internal error', code: 'internal' } })
      }),
  })
  beginSession(2)
  const on = enablePush(2).catch(() => {})
  await vi.waitFor(() => expect(localStorage.getItem('taskto.push')).toBe('2'))
  const leaving = leavePush(1)
  refuse()
  await on
  await leaving
  expect(localStorage.getItem('taskto.push')).toBeNull()
})

test("turning push off ends a subscription whose flag was another account's before the click", async () => {
  const sub = fakeSub()
  pushBrowser({ permission: 'granted', subscription: sub })
  localStorage.setItem('taskto.push', '2')
  const calls = fakeFetch({ 'DELETE /api/me/push/subscription': { status: 204 } })
  await disablePush()
  expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(1)
  expect(sub.unsubscribe).toHaveBeenCalled()
  expect(localStorage.getItem('taskto.push')).toBeNull()
})

test('a tab closed while turning push off leaves it off: shown off elsewhere, and never registered again at a start', async () => {
  const held = heldUnsubscribe()
  pushBrowser({ permission: 'granted', subscription: held.sub })
  localStorage.setItem('taskto.push', '1')
  const calls = fakeFetch({
    'GET /api/me/push/key': { body: { public_key: SERVER_KEY } },
    'DELETE /api/me/push/subscription': { status: 204 },
    'PUT /api/me/push/subscription': { status: 204 },
  })
  const off = disablePush().catch(() => {})
  try {
    await vi.waitFor(() => expect(held.sub.unsubscribe).toHaveBeenCalled())
    expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(1)
    const later = Date.now() + 61_000
    vi.spyOn(Date, 'now').mockReturnValue(later)
    vi.resetModules()
    const other = await import('./push')
    expect(await other.pushState({ platform: 'other', installed: false } as never)).toBe('off')
    expect(await other.syncPush(1)).toBe(false)
    expect(calls.some((c) => c.method === 'PUT')).toBe(false)
  } finally {
    held.end()
    await off
  }
})

test('without Web Locks, two tabs ending the subscription at once never rewrite each other’s mark', async () => {
  const first = heldUnsubscribe()
  const browser = pushBrowser({ permission: 'granted', subscription: first.sub })
  fakeFetch({
    'GET /api/me/push/key': { body: { public_key: SERVER_KEY } },
    'PUT /api/me/push/subscription': { status: 204 },
  })
  localStorage.setItem('taskto.push', '9')
  const leaving = leavePush(9)
  await vi.waitFor(() => expect(first.sub.unsubscribe).toHaveBeenCalled())
  vi.resetModules()
  const other = await import('./push')
  const second = heldUnsubscribe()
  browser.state.subscription = second.sub
  const written: string[] = []
  const set = Storage.prototype.setItem
  const remove = Storage.prototype.removeItem
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation(function (this: Storage, key: string, value: string) {
    written.push(key)
    set.call(this, key, value)
  })
  vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(function (this: Storage, key: string) {
    written.push(key)
    remove.call(this, key)
  })
  const otherLeaving = other.leavePush(9)
  try {
    await vi.waitFor(() => expect(second.sub.unsubscribe).toHaveBeenCalled())
    const theirs = written.filter((k) => k.startsWith('taskto.push-unsubscribing'))
    expect(theirs.length).toBeGreaterThan(0)
    written.length = 0
    first.end()
    await leaving
    expect(written.filter((k) => theirs.includes(k))).toEqual([])
    await expect(enablePush(1)).rejects.toBeInstanceOf(PushBusy)
  } finally {
    first.end()
    second.end()
    await leaving
    await otherLeaving
  }
})

test('without Web Locks, a start that registers the device again stops when another tab turned push off meanwhile', async () => {
  const sub = fakeSub()
  pushBrowser({ permission: 'granted', subscription: sub })
  localStorage.setItem('taskto.push', '1')
  const calls = fakeFetch({ 'PUT /api/me/push/subscription': { status: 204 } })
  const registration = await navigator.serviceWorker.getRegistration()
  let answer: (() => void) | undefined
  vi.mocked(registration?.pushManager.getSubscription as () => Promise<unknown>).mockImplementationOnce(
    () =>
      new Promise((done) => {
        answer = () => done(sub)
      }),
  )
  const synced = syncPush(1)
  await vi.waitFor(() => expect(answer).toBeDefined())
  await new Promise((r) => setTimeout(r, 0))
  localStorage.setItem('taskto.push', 'off:1')
  answer?.()
  expect(await synced).toBe(false)
  expect(calls.some((c) => c.method === 'PUT')).toBe(false)
})

test("a sign-out's removal of push stops when a newer login of the account started while it waited", async () => {
  const sub = fakeSub()
  pushBrowser({ permission: 'granted', subscription: sub })
  localStorage.setItem('taskto.push', '1')
  const registration = await navigator.serviceWorker.getRegistration()
  let answer: (() => void) | undefined
  vi.mocked(registration?.pushManager.getSubscription as () => Promise<unknown>).mockImplementationOnce(
    () =>
      new Promise((done) => {
        answer = () => done(sub)
      }),
  )
  let older = true
  const leaving = leavePush(1, () => older)
  await vi.waitFor(() => expect(answer).toBeDefined())
  older = false
  localStorage.setItem('taskto.push', '1')
  answer?.()
  await leaving
  expect(sub.unsubscribe).not.toHaveBeenCalled()
  expect(localStorage.getItem('taskto.push')).toBe('1')
})

test('marks of unsubscribes long over, or that are no time at all, are removed when looked at', async () => {
  pushBrowser({ permission: 'granted', subscription: fakeSub() })
  fakeFetch({ 'PUT /api/me/push/subscription': { status: 204 } })
  localStorage.setItem('taskto.push', '1')
  localStorage.setItem('taskto.push-unsubscribing:closed-tab', String(Date.now() - 120_000))
  localStorage.setItem('taskto.push-unsubscribing:garbled', 'not a time')
  expect(await syncPush(1)).toBe(true)
  expect(localStorage.getItem('taskto.push-unsubscribing:closed-tab')).toBeNull()
  expect(localStorage.getItem('taskto.push-unsubscribing:garbled')).toBeNull()
})

test("without Web Locks, a turn-off that fails never puts push back on over another tab's turn-off", async () => {
  pushBrowser({ permission: 'granted', subscription: fakeSub() })
  localStorage.setItem('taskto.push', '1')
  let deletes = 0
  let fail: () => void = () => {}
  fakeFetch({
    'DELETE /api/me/push/subscription': () => {
      deletes++
      return deletes === 1
        ? new Promise((done) => {
            fail = () => done({ status: 500, body: { error: 'internal error', code: 'internal' } })
          })
        : new Promise(() => {})
    },
  })
  const mine = disablePush().catch(() => {})
  await vi.waitFor(() => expect(deletes).toBe(1))
  vi.resetModules()
  const other = await import('./push')
  ;(await import('../session')).beginSession(1)
  void other.disablePush().catch(() => {})
  await vi.waitFor(() => expect(deletes).toBe(2))
  fail()
  await mine
  expect(localStorage.getItem('taskto.push')).not.toBe('1')
})

test('without Web Locks, two turn-offs that both fail leave push on as it was, registered again at a start', async () => {
  pushBrowser({ permission: 'granted', subscription: fakeSub() })
  localStorage.setItem('taskto.push', '1')
  const fails: (() => void)[] = []
  const calls = fakeFetch({
    'DELETE /api/me/push/subscription': () =>
      new Promise((done) => {
        fails.push(() => done({ status: 500, body: { error: 'internal error', code: 'internal' } }))
      }),
    'PUT /api/me/push/subscription': { status: 204 },
  })
  const first = disablePush().catch(() => {})
  await vi.waitFor(() => expect(fails).toHaveLength(1))
  vi.resetModules()
  const other = await import('./push')
  ;(await import('../session')).beginSession(1)
  const second = other.disablePush().catch(() => {})
  await vi.waitFor(() => expect(fails).toHaveLength(2))
  fails[0]()
  await first
  fails[1]()
  await second
  expect(localStorage.getItem('taskto.push')).toBe('1')
  expect(await other.syncPush(1)).toBe(true)
  expect(calls.some((c) => c.method === 'PUT')).toBe(true)
})

test('without Web Locks, a turn-off that fails while an earlier one is still on its way leaves push off', async () => {
  pushBrowser({ permission: 'granted', subscription: fakeSub() })
  localStorage.setItem('taskto.push', '1')
  const fails: (() => void)[] = []
  const calls = fakeFetch({
    'DELETE /api/me/push/subscription': () =>
      new Promise((done) => {
        fails.push(() => done({ status: 500, body: { error: 'internal error', code: 'internal' } }))
      }),
    'PUT /api/me/push/subscription': { status: 204 },
  })
  const first = disablePush().catch(() => {})
  await vi.waitFor(() => expect(fails).toHaveLength(1))
  vi.resetModules()
  const other = await import('./push')
  ;(await import('../session')).beginSession(1)
  const second = other.disablePush().catch(() => {})
  await vi.waitFor(() => expect(fails).toHaveLength(2))
  fails[1]()
  await second
  expect(localStorage.getItem('taskto.push')).not.toBe('1')
  expect(await other.syncPush(1)).toBe(false)
  expect(calls.some((c) => c.method === 'PUT')).toBe(false)
  fails[0]()
  await first
  expect(localStorage.getItem('taskto.push')).toBe('1')
})

test("turning push off over another account's stale flag never ends that account's push turned on again meanwhile", async () => {
  const sub = fakeSub()
  pushBrowser({ permission: 'granted', subscription: sub })
  localStorage.setItem('taskto.push', '2')
  let answer: (() => void) | undefined
  fakeFetch({
    'DELETE /api/me/push/subscription': () =>
      new Promise((done) => {
        answer = () => done({ status: 204 })
      }),
  })
  const off = disablePush()
  await vi.waitFor(() => expect(answer).toBeDefined())
  localStorage.setItem('taskto.push', '2')
  answer?.()
  await off
  expect(sub.unsubscribe).not.toHaveBeenCalled()
  expect(localStorage.getItem('taskto.push')).toBe('2')
})

test("turning push off over another account's stale flag, with no subscription here, leaves that account's flag", async () => {
  pushBrowser({ permission: 'granted', subscription: null })
  localStorage.setItem('taskto.push', '2')
  const registration = await navigator.serviceWorker.getRegistration()
  let answer: ((sub: null) => void) | undefined
  vi.mocked(registration?.pushManager.getSubscription as () => Promise<unknown>).mockImplementationOnce(
    () =>
      new Promise((done) => {
        answer = done
      }),
  )
  const off = disablePush()
  await vi.waitFor(() => expect(answer).toBeDefined())
  localStorage.setItem('taskto.push', '2')
  answer?.(null)
  await off
  expect(localStorage.getItem('taskto.push')).toBe('2')
})
