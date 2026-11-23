// The service worker's behaviour, run against fakes of the worker's globals: the template as
// the build renders it, with a precache list.

import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { beforeEach, expect, test, vi } from 'vitest'
import { renderServiceWorker } from './plugin'

const template = readFileSync(join(import.meta.dirname, 'sw.template.js'), 'utf8')
const PRECACHE = ['/assets/AppsPage-def456.js', '/assets/index-abc123.js', '/icons/icon-192.png', '/index.html']

type Listener = (event: unknown) => void

// An in-memory CacheStorage, keys in the order the caches were created.
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
    clients: { claim: vi.fn(async () => {}) },
  }
  const fetchCalls: { url: string; cache?: string }[] = []
  const fakeFetch = async (input: Request | string) => {
    const request = typeof input === 'string' ? new Request(input) : input
    fetchCalls.push({ url: new URL(request.url).pathname, cache: request.cache })
    return network(new URL(request.url).pathname, undefined)
  }
  // In a worker a relative URL resolves against its own location.
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
  // A page request; returns what the worker answered, or "pass" when it left it to the browser.
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
  return { self, caches, fetchCalls, dispatchWait, request, listeners }
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
  // v1 is active, with tabs open; v2 installed and waited, then v3 installed over it.
  await (await caches.open('taskto-shell-v1')).put('/__taskto-activated', new Response(''))
  await (await caches.open('taskto-shell-v1')).put('/assets/Apps-v1.js', new Response('v1 chunk'))
  await caches.open('taskto-shell-v2')
  const v3 = boot('v3', async () => new Response('missing', { status: 404 }), caches)
  await caches.open('taskto-shell-v3')
  await v3.dispatchWait('activate')
  expect(await caches.keys()).toEqual(['taskto-shell-v1', 'taskto-shell-v3'])
  // A v1 tab, now controlled by v3, still gets its lazy chunk.
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
