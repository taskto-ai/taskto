import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { beforeEach, expect, test, vi } from 'vitest'
import { renderServiceWorker } from './plugin'

const template = readFileSync(join(import.meta.dirname, 'sw.template.js'), 'utf8')
const PRECACHE = ['/assets/AppsPage-def456.js', '/assets/index-abc123.js', '/icons/icon-192.png', '/index.html']

type Listener = (event: unknown) => void

class FakeCache {
  entries = new Map<string, Response>()
  async put(key: string | Request, response: Response) {
    this.entries.set(typeof key === 'string' ? key : new URL(key.url).pathname, response)
  }
  async match(key: string) {
    return this.entries.get(key)?.clone()
  }
}
class FakeCaches {
  all = new Map<string, FakeCache>()
  async open(name: string) {
    if (!this.all.has(name)) this.all.set(name, new FakeCache())
    return this.all.get(name) as FakeCache
  }
  async keys() {
    return [...this.all.keys()]
  }
  async delete(name: string) {
    return this.all.delete(name)
  }
  async match(key: string) {
    for (const c of this.all.values()) {
      const hit = await c.match(key)
      if (hit) return hit
    }
    return undefined
  }
}

function boot(
  version = 'v1',
  network: (url: string, init?: RequestInit) => Promise<Response> = async (url) => new Response(`body of ${url}`),
  caches = new FakeCaches(),
) {
  const listeners = new Map<string, Listener>()
  const self = {
    location: { origin: 'https://tasks.example.com' },
    addEventListener: (type: string, l: Listener) => listeners.set(type, l),
    skipWaiting: vi.fn(),
    clients: {
      claim: vi.fn(async () => {}),
      matchAll: vi.fn(async (): Promise<unknown[]> => []),
      openWindow: vi.fn(async (_path: string) => null),
    },
    registration: { showNotification: vi.fn(async (_title: string, _options: unknown) => {}) },
  }
  const fetchCalls: { url: string; cache?: string }[] = []
  const fakeFetch = async (input: Request | string) => {
    const request = typeof input === 'string' ? new Request(input) : input
    fetchCalls.push({ url: new URL(request.url).pathname, cache: request.cache })
    return network(new URL(request.url).pathname, undefined)
  }
  class WorkerRequest extends Request {
    constructor(input: string | Request, init?: RequestInit) {
      super(typeof input === 'string' ? new URL(input, self.location.origin).href : input, init)
    }
  }
  const code = renderServiceWorker(template, PRECACHE, version)
  new Function('self', 'caches', 'fetch', 'Request', 'URL', code)(self, caches, fakeFetch, WorkerRequest, URL)

  const dispatchWait = async (type: string) => {
    let work: Promise<unknown> = Promise.resolve()
    listeners.get(type)?.({ waitUntil: (p: Promise<unknown>) => (work = p) })
    await work
  }
  const request = async (
    path: string,
    { method = 'GET', mode = 'cors', origin = 'https://tasks.example.com' } = {},
  ) => {
    const req = new Request(origin + path, { method })
    Object.defineProperty(req, 'mode', { value: mode })
    let answer: Promise<Response> | undefined
    listeners.get('fetch')?.({ request: req, respondWith: (p: Promise<Response>) => (answer = p) })
    return answer ? await answer : 'pass'
  }
  const emit = async (type: string, event: Record<string, unknown>) => {
    let work: Promise<unknown> = Promise.resolve()
    listeners.get(type)?.({ ...event, waitUntil: (p: Promise<unknown>) => (work = p) })
    await work
  }
  return { self, caches, fetchCalls, dispatchWait, request, listeners, emit }
}

const text = async (r: Response | 'pass') => (r === 'pass' ? r : r.text())

let sw: ReturnType<typeof boot>
beforeEach(() => {
  sw = boot()
})

test('install downloads the whole app, the lazy chunks too, past the HTTP cache', async () => {
  await sw.dispatchWait('install')
  const cache = await sw.caches.open('taskto-shell-v1')
  expect([...cache.entries.keys()].sort()).toEqual(PRECACHE)
  expect(sw.fetchCalls.map((c) => c.url).sort()).toEqual(PRECACHE)
  expect(sw.fetchCalls.every((c) => c.cache === 'reload')).toBe(true)
})

test('install fails, leaving no half cache, when one file cannot be downloaded', async () => {
  const broken = boot('v1', async (url) =>
    url.includes('AppsPage') ? new Response('', { status: 404 }) : new Response('x'),
  )
  await expect(broken.dispatchWait('install')).rejects.toThrow(/AppsPage/)
  expect(await broken.caches.keys()).toEqual([])
})

test('an update waits: it does not take over by itself, and does when asked', async () => {
  await sw.dispatchWait('install')
  expect(sw.self.skipWaiting).not.toHaveBeenCalled()
  sw.listeners.get('message')?.({ data: { type: 'SKIP_WAITING' } })
  expect(sw.self.skipWaiting).toHaveBeenCalledOnce()
  sw.listeners.get('message')?.({ data: { type: 'something else' } })
  expect(sw.self.skipWaiting).toHaveBeenCalledOnce()
})

test('activate keeps this version and the previous active one, drops older ones, and claims', async () => {
  const caches = new FakeCaches()
  for (const old of ['taskto-shell-v0', 'taskto-shell-v1']) {
    await (await caches.open(old)).put('/__taskto-activated', new Response(''))
  }
  await caches.open('other-app-cache')
  const v2 = boot('v2', undefined, caches)
  await caches.open('taskto-shell-v2')
  await v2.dispatchWait('activate')
  expect(await caches.keys()).toEqual(['taskto-shell-v1', 'other-app-cache', 'taskto-shell-v2'])
  expect(v2.self.clients.claim).toHaveBeenCalled()
})

test('two deploys before an update: the version tabs still run keeps its cache', async () => {
  const caches = new FakeCaches()
  await (await caches.open('taskto-shell-v1')).put('/__taskto-activated', new Response(''))
  await (await caches.open('taskto-shell-v1')).put('/assets/Apps-v1.js', new Response('v1 chunk'))
  await caches.open('taskto-shell-v2')
  const v3 = boot('v3', async () => new Response('missing', { status: 404 }), caches)
  await caches.open('taskto-shell-v3')
  await v3.dispatchWait('activate')
  expect(await caches.keys()).toEqual(['taskto-shell-v1', 'taskto-shell-v3'])
  expect(await text(await v3.request('/assets/Apps-v1.js'))).toBe('v1 chunk')
})

test('precached files are answered from the cache, and an old tab still gets its old chunks', async () => {
  const caches = new FakeCaches()
  await (await caches.open('taskto-shell-v0')).put('/assets/Old-111.js', new Response('old chunk'))
  const v1 = boot('v1', async () => new Response('from the network'), caches)
  await v1.dispatchWait('install')
  await (await caches.open('taskto-shell-v1')).put('/assets/index-abc123.js', new Response('cached'))
  expect(await text(await v1.request('/assets/index-abc123.js'))).toBe('cached')
  expect(await text(await v1.request('/assets/Old-111.js'))).toBe('old chunk')
})

test("the worker file and other routes are the browser's; an unknown asset goes to the network, uncached", async () => {
  await sw.dispatchWait('install')
  expect(await sw.request('/sw.js')).toBe('pass')
  expect(await sw.request('/some/other/file.json')).toBe('pass')
  expect(await text(await sw.request('/assets/unknown-zzz.js'))).toBe('body of /assets/unknown-zzz.js')
  expect([...(await sw.caches.open('taskto-shell-v1')).entries.keys()].sort()).toEqual(PRECACHE)
})

test("never answers, or caches, the server's routes", async () => {
  await sw.dispatchWait('install')
  const before = [...(await sw.caches.open('taskto-shell-v1')).entries.keys()]
  for (const path of [
    '/api/me',
    '/api/workspaces/1/conversations',
    '/api/workspaces/1/spaces/2/apps/3/offline',
    '/api/workspaces/1/spaces/2/apps/3/uploads/abc',
    '/mcp',
    '/cli-mcp',
    '/healthz',
    '/healthz/backup',
  ]) {
    expect(await sw.request(path), path).toBe('pass')
    expect(await sw.request(path, { mode: 'navigate' }), `navigate ${path}`).toBe('pass')
  }
  expect([...(await sw.caches.open('taskto-shell-v1')).entries.keys()]).toEqual(before)
})

test('answers only GET requests of its own origin', async () => {
  await sw.dispatchWait('install')
  expect(await sw.request('/assets/index-abc123.js', { method: 'POST' })).toBe('pass')
  expect(await sw.request('/assets/index-abc123.js', { origin: 'https://cdn.example.com' })).toBe('pass')
})

test("a page navigation goes to the network, so a link to an app route is the server's page", async () => {
  await sw.dispatchWait('install')
  const answer = await sw.request('/w/4/boards', { mode: 'navigate' })
  expect(await text(answer)).toBe('body of /w/4/boards')
})

test('a server error on a navigation is shown as it is, not replaced by the cached page', async () => {
  const down = boot('v1', async (url) =>
    PRECACHE.includes(url) ? new Response('shell') : new Response('boom', { status: 502 }),
  )
  await down.dispatchWait('install')
  const answer = (await down.request('/w/1/apps/3', { mode: 'navigate' })) as Response
  expect(answer.status).toBe(502)
})

test('offline, an app route opens the cached page; a server route is not answered', async () => {
  let online = true
  const flaky = boot('v1', async (url) => {
    if (!online) throw new TypeError('Failed to fetch')
    return new Response(`body of ${url}`)
  })
  await flaky.dispatchWait('install')
  online = false
  expect(await text(await flaky.request('/w/4/boards', { mode: 'navigate' }))).toBe('body of /index.html')
  expect(await text(await flaky.request('/invite', { mode: 'navigate' }))).toBe('body of /index.html')
  expect(await flaky.request('/api/me', { mode: 'navigate' })).toBe('pass')
})

const WORDS = {
  card_assigned: 'A card was assigned to you',
  card_due: 'A card of yours is due',
  card_overdue: 'A card of yours is overdue',
  app_effect_failed: 'An app action needs your attention',
  app_reminder: 'A reminder from one of your apps',
  changed: 'Something changed',
}
const pushOf = (data: unknown) => ({ data: { json: () => (typeof data === 'function' ? data() : data) } })
const shown = (w: ReturnType<typeof boot>) => w.self.registration.showNotification.mock.calls

test('a push shows the brand and the words for its kind, tagged and carrying only its ID', async () => {
  await sw.emit('message', { data: { type: 'PUSH_STRINGS', strings: WORDS } })
  await sw.emit('push', pushOf({ id: 42, kind: 'card_assigned' }))
  expect(shown(sw)).toHaveLength(1)
  const [title, options] = shown(sw)[0] as [string, Record<string, unknown>]
  expect(title).toBe('taskto.ai')
  expect(options).toEqual({
    body: 'A card was assigned to you',
    tag: 'taskto-notification-42',
    data: { id: 42 },
    icon: '/icons/icon-192.png',
  })
})

test('a reminder from an app is a push of generic words that never carry the record', async () => {
  await sw.emit('message', { data: { type: 'PUSH_STRINGS', strings: WORDS } })
  await sw.emit('push', pushOf({ id: 9, kind: 'app_reminder', title: 'Pay the rent', record: 4 }))
  const [title, options] = shown(sw)[0] as [string, Record<string, unknown>]
  expect(title).toBe('taskto.ai')
  expect(options).toEqual({
    body: 'A reminder from one of your apps',
    tag: 'taskto-notification-9',
    data: { id: 9 },
    icon: '/icons/icon-192.png',
  })
  expect(JSON.stringify(shown(sw))).not.toContain('Pay the rent')
})

test('a push shows something whatever it carries: unknown kinds and unreadable data say only the brand and a generic line', async () => {
  await sw.emit('push', pushOf({ id: 7, kind: 'card_assigned' }))
  expect((shown(sw)[0] as [string, { body: string }])[1].body).toBe('')
  await sw.emit('message', { data: { type: 'PUSH_STRINGS', strings: WORDS } })
  await sw.emit('push', pushOf({ id: 8, kind: 'something_new', title: 'Secret plan' }))
  await sw.emit(
    'push',
    pushOf(() => {
      throw new SyntaxError('not JSON')
    }),
  )
  await sw.emit('push', { data: null })
  await sw.emit('push', pushOf({ id: -3, kind: 'changed' }))
  await sw.emit('push', pushOf({ id: '5; drop', kind: 'changed' }))
  const calls = shown(sw) as [string, { body: string; tag: string; data: { id: number } }][]
  expect(calls).toHaveLength(6)
  for (const call of calls.slice(1)) {
    expect(call[0]).toBe('taskto.ai')
    expect(call[1].body).toBe('Something changed')
  }
  expect(calls.slice(1).map((c) => c[1].data.id)).toEqual([8, 0, 0, 0, 0])
  expect(JSON.stringify(calls)).not.toContain('Secret plan')
})

test('the words a page gives are kept only for the kinds the server sends, and short', async () => {
  await sw.emit('message', {
    data: { type: 'PUSH_STRINGS', strings: { ...WORDS, changed: 'x'.repeat(201), extra: 'ignored', card_assigned: 5 } },
  })
  await sw.emit('message', { data: { type: 'PUSH_STRINGS', strings: 'nope' } })
  await sw.emit('push', pushOf({ id: 1, kind: 'card_assigned' }))
  await sw.emit('push', pushOf({ id: 2, kind: 'changed' }))
  await sw.emit('push', pushOf({ id: 3, kind: 'app_effect_failed' }))
  const bodies = (shown(sw) as [string, { body: string }][]).map((c) => c[1].body)
  expect(bodies).toEqual(['', '', 'An app action needs your attention'])
  expect(await sw.caches.keys()).toEqual(['taskto-push-strings'])
})

test('a push of a kind that a cache made before it existed has no words for says the generic line', async () => {
  const { card_due: _due, card_overdue: _overdue, ...old } = WORDS
  await sw.emit('message', { data: { type: 'PUSH_STRINGS', strings: old } })
  await sw.emit('push', pushOf({ id: 1, kind: 'card_due' }))
  await sw.emit('push', pushOf({ id: 2, kind: 'card_overdue' }))
  await sw.emit('push', pushOf({ id: 3, kind: 'card_assigned' }))
  const bodies = (shown(sw) as [string, { body: string }][]).map((c) => c[1].body)
  expect(bodies).toEqual(['Something changed', 'Something changed', 'A card was assigned to you'])
})

test('a kind the saved words lack (kept from before an update) says the generic line', async () => {
  await sw.emit('message', { data: { type: 'PUSH_STRINGS', strings: WORDS } })
  await sw.emit('push', pushOf({ id: 1, kind: 'card_comment' }))
  await sw.emit('push', pushOf({ id: 2, kind: 'card_mention' }))
  const bodies = (shown(sw) as [string, { body: string }][]).map((c) => c[1].body)
  expect(bodies).toEqual(['Something changed', 'Something changed'])
})

test('a push tells the open pages, which ask the inbox again', async () => {
  const page = { url: 'https://tasks.example.com/w/1/home', postMessage: vi.fn() }
  sw.self.clients.matchAll.mockResolvedValue([page])
  await sw.emit('push', pushOf({ id: 1, kind: 'changed' }))
  expect(page.postMessage).toHaveBeenCalledWith({ type: 'PUSH_RECEIVED' })
})

test('clicking a push opens the app at the notification, in a new window when none is open', async () => {
  const close = vi.fn()
  await sw.emit('notificationclick', { notification: { close, data: { id: 42 } } })
  expect(close).toHaveBeenCalled()
  expect(sw.self.clients.openWindow).toHaveBeenCalledWith('/notification/42')
})

test('clicking a push focuses the open app and tells it where to go, without reloading it', async () => {
  const page = { url: 'https://tasks.example.com/w/1/boards', focus: vi.fn(async () => {}), postMessage: vi.fn() }
  const other = { url: 'https://other.example.org/', focus: vi.fn(async () => {}), postMessage: vi.fn() }
  sw.self.clients.matchAll.mockResolvedValue([other, page])
  await sw.emit('notificationclick', { notification: { close: vi.fn(), data: { id: 9 } } })
  expect(page.focus).toHaveBeenCalled()
  expect(page.postMessage).toHaveBeenCalledWith({ type: 'OPEN', path: '/notification/9' })
  expect(other.postMessage).not.toHaveBeenCalled()
  expect(sw.self.clients.openWindow).not.toHaveBeenCalled()
})

test('a click on a notification with no usable ID opens the app, never a path made from the data', async () => {
  for (const data of [undefined, {}, { id: 0 }, { id: '../../api/me' }, { id: 1.5 }]) {
    await sw.emit('notificationclick', { notification: { close: vi.fn(), data } })
  }
  expect(sw.self.clients.openWindow.mock.calls.map((c) => c[0])).toEqual(['/', '/', '/', '/', '/'])
})

test('a push never makes the worker answer or store anything from the server', async () => {
  await sw.dispatchWait('install')
  const before = [...(await sw.caches.open('taskto-shell-v1')).entries.keys()]
  await sw.emit('push', pushOf({ id: 1, kind: 'changed' }))
  expect([...(await sw.caches.open('taskto-shell-v1')).entries.keys()]).toEqual(before)
  expect(await sw.request('/notification/1', { mode: 'navigate' })).toBeInstanceOf(Response)
  expect(await sw.request('/api/me/notifications/1')).toBe('pass')
})
