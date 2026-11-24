// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { App } from '../App'
import { resetInstallInfo } from '../pwa/install'
import { NotificationsTab } from '../settings/NotificationsTab'
import { Shell } from '../shell/Shell'
import { fakeFetch } from '../test/fakes'
import { NotificationsPage } from './NotificationsPage'
import { syncPush } from './push'

const SERVER_KEY = 'AQID' // the server's public key, as the push endpoint gives it: 1, 2, 3

const me = {
  user: { id: 1, email: 'owner@example.com', name: 'Owner' },
  workspaces: [{ id: 1, name: 'Home', role: 'admin' as const }],
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

// A browser that can push, as far as the page can tell: the worker's registration, the push
// manager, and the Notification permission, which the test sets.
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
})
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  Reflect.deleteProperty(navigator, 'serviceWorker')
})

const pushCalls = (calls: { path: string }[]) => calls.filter((c) => c.path.startsWith('/api/me/push'))

// --- Settings: push is optional and explicit ------------------------------------------------

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
  // Subscribed with the server's key, for visible notifications only.
  const options = browser.subscribe.mock.calls[0]?.[0] as unknown as {
    userVisibleOnly: boolean
    applicationServerKey: Uint8Array
  }
  expect(options.userVisibleOnly).toBe(true)
  expect([...options.applicationServerKey]).toEqual([1, 2, 3])
  // The endpoint and keys, and not the browser's other fields (the API refuses unknown ones).
  const put = calls.find((c) => c.method === 'PUT')
  expect(put?.path).toBe('/api/me/push/subscription')
  expect(put?.body).toEqual({
    endpoint: 'https://push.example.net/abc',
    keys: { p256dh: 'device-key', auth: 'device-auth' },
  })
  // The service worker is given the words for a push, in this language.
  expect(browser.postMessage).toHaveBeenCalledWith({
    type: 'PUSH_STRINGS',
    strings: {
      card_assigned: 'A card was assigned to you',
      card_due: 'A card of yours is due',
      card_overdue: 'A card of yours is overdue',
      card_comment: 'A card of yours has a new comment',
      card_mention: 'You were mentioned in a comment',
      app_effect_failed: 'An app action needs your attention',
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

// --- Starting the app signed in, and signing out --------------------------------------------

test('a device whose person turned push on is registered again at start; another person never gets it', async () => {
  const sub = fakeSub()
  pushBrowser({ permission: 'granted', subscription: sub })
  const calls = fakeFetch({ 'PUT /api/me/push/subscription': { status: 204 } })

  // Never turned on: nothing.
  expect(await syncPush(1)).toBe(false)
  // Turned on by someone else on this browser: not registered for this person.
  localStorage.setItem('taskto.push', '2')
  expect(await syncPush(1)).toBe(false)
  expect(pushCalls(calls)).toEqual([])
  // Turned on by this person: registered again (a new login is a new session).
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

test('signing out removes this device first, while the session can still say whose it is', async () => {
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
  await act(async () => {
    fireEvent.click(screen.getByRole('button', { name: 'Log out' }))
  })
  await screen.findByRole('heading', { name: 'Log in' })
  const order = calls
    .filter((c) => c.method === 'DELETE' || c.path === '/api/logout')
    .map((c) => `${c.method} ${c.path}`)
  expect(order).toEqual(['DELETE /api/me/push/subscription', 'POST /api/logout'])
  expect(sub.unsubscribe).toHaveBeenCalled()
  expect(localStorage.getItem('taskto.push')).toBeNull()
})

// --- The inbox works in every browser -------------------------------------------------------

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
  // This browser has no push, and the page doesn't mind: it offers the settings.
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

// --- The shell: the bell, and the address a push opens --------------------------------------

test('the top bar has a bell with the unread count, which opens the inbox', async () => {
  window.history.replaceState(null, '', '/w/1/home')
  fakeFetch({
    'GET /api/workspaces/1/notifications/count': { body: { unread: 120 } },
    'GET /api/workspaces/1/notifications': { body: { notifications: [], more: false, unread: 120 } },
    'GET /api/workspaces/1/home': { body: { items: [] } },
  })
  render(<Shell me={me} onLogout={() => {}} />)
  const bell = await screen.findByRole('link', { name: 'Notifications, 120 unread' })
  expect(bell.getAttribute('href')).toBe('/w/1/notifications')
  expect(bell.textContent).toBe('99+')
  fireEvent.click(bell)
  await screen.findByRole('heading', { name: 'Notifications' })
})

test('a server without an inbox, or one that fails, leaves the bell quiet and the app usable', async () => {
  window.history.replaceState(null, '', '/w/1/knowledge')
  fakeFetch({ 'GET /api/workspaces/1/spaces': { body: { spaces: [] } } })
  render(<Shell me={me} onLogout={() => {}} />)
  const bell = await screen.findByRole('link', { name: 'Notifications' })
  expect(bell.textContent).toBe('')
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
  // Still the login: nothing was read as nobody.
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
  // The app listened while it was loading and stopped when this screen came up, in an effect
  // that can run just after the screen is drawn: settle the effects, or the listener that is
  // about to be removed is still counted as listening.
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

// --- Turning push off, with the server or the browser failing ---------------------------------

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
  // Nothing was changed on this device, and the page still says push is on.
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
  kinds: { card_assigned: 'off', app_effect_failed: 'off' },
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
  // The first summary takes the person's own time zone along.
  expect(puts[1]?.kinds).toEqual({ app_effect_failed: 'digest' })
  const first = puts[1]?.digest as { hour: number; timezone: string } | undefined
  expect(first?.hour).toBe(8)
  expect(puts[2]).toEqual({ digest: { hour: 18, timezone: first?.timezone } })
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
  let saved = emailChoices({ available: false, kinds: { card_assigned: 'instant', app_effect_failed: 'off' } })
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
  // Nothing can be turned on, and off is still there.
  const options = within(assigned).getAllByRole('option') as HTMLOptionElement[]
  expect(options.filter((o) => o.disabled).map((o) => o.value)).toEqual(['instant', 'digest'])
  fireEvent.change(assigned, { target: { value: 'off' } })
  // With nothing on there is nothing left to turn off: the panel says email can't be set up.
  await screen.findByText(/so notifications can't be sent by email/)
  expect(screen.queryByLabelText('A card was assigned to you')).toBeNull()
  expect(calls.filter((c) => c.method === 'PUT').map((c) => c.body)).toEqual([{ kinds: { card_assigned: 'off' } }])
})

test('the panel says whether the address is confirmed and sends the link', async () => {
  pushBrowser({ permission: 'denied' })
  let saved = emailChoices({
    address: { email: 'owner@example.com', state: 'unverified' },
    kinds: { card_assigned: 'instant', app_effect_failed: 'off' },
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

test("a time zone the person picked is not replaced by the browser's when a summary is turned on again", async () => {
  pushBrowser({ permission: 'denied' })
  vi.spyOn(Intl.DateTimeFormat.prototype, 'resolvedOptions').mockReturnValue({
    timeZone: 'America/New_York',
  } as Intl.ResolvedDateTimeFormatOptions)
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
  const first = render(<NotificationsTab me={1} workspace={1} />)
  let assigned = (await screen.findByLabelText('A card was assigned to you')) as HTMLSelectElement
  fireEvent.change(assigned, { target: { value: 'digest' } })
  const zone = (await screen.findByLabelText('Time zone')) as HTMLSelectElement
  await waitFor(() => expect(zone.value).toBe('America/New_York'))
  fireEvent.change(zone, { target: { value: 'UTC' } })
  await waitFor(() => expect(saved.digest.timezone).toBe('UTC'))
  fireEvent.change(assigned, { target: { value: 'off' } })
  await waitFor(() => expect(saved.kinds.card_assigned).toBe('off'))
  // Leaving the page and coming back forgets nothing: the server says the zone was chosen.
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
