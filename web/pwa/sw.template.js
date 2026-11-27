const VERSION = '__VERSION__'
const PRECACHE = ['__PRECACHE__']

const PREFIX = 'taskto-shell-'
const CACHE = PREFIX + VERSION
const SHELL = '/index.html'
const SERVER_PATHS = ['/api', '/mcp', '/cli-mcp', '/healthz']

function isServerPath(pathname) {
  return SERVER_PATHS.some((p) => pathname === p || pathname.startsWith(`${p}/`))
}

self.addEventListener('install', (event) => {
  event.waitUntil(precache())
})

async function precache() {
  const cache = await caches.open(CACHE)
  try {
    await Promise.all(
      PRECACHE.map(async (url) => {
        const response = await fetch(new Request(url, { cache: 'reload', credentials: 'same-origin' }))
        if (!response.ok) throw new Error(`${url}: ${response.status}`)
        await cache.put(url, response)
      }),
    )
  } catch (e) {
    await caches.delete(CACHE)
    throw e
  }
}

self.addEventListener('activate', (event) => {
  event.waitUntil(cleanUp())
})

const ACTIVATED = '/__taskto-activated'

async function cleanUp() {
  await (await caches.open(CACHE)).put(ACTIVATED, new Response(''))
  const ours = (await caches.keys()).filter((k) => k.startsWith(PREFIX))
  let previous = null
  for (const k of ours) {
    if (k !== CACHE && (await (await caches.open(k)).match(ACTIVATED))) previous = k
  }
  await Promise.all(ours.filter((k) => k !== CACHE && k !== previous).map((k) => caches.delete(k)))
  await self.clients.claim()
}

self.addEventListener('message', (event) => {
  const data = event.data
  if (!data) return
  if (data.type === 'SKIP_WAITING') self.skipWaiting()
  if (data.type === 'PUSH_STRINGS') event.waitUntil(keepStrings(data.strings))
})

const BRAND = 'taskto.ai'
const KINDS = [
  'card_assigned',
  'card_comment',
  'card_mention',
  'card_due',
  'card_overdue',
  'app_effect_failed',
  'changed',
]
const STRINGS_CACHE = 'taskto-push-strings'
const STRINGS_KEY = '/__taskto-push-strings'
const MAX_STRING = 200

async function keepStrings(strings) {
  if (!strings || typeof strings !== 'object') return
  const clean = {}
  for (const kind of KINDS) {
    if (typeof strings[kind] === 'string' && strings[kind].length <= MAX_STRING) clean[kind] = strings[kind]
  }
  const cache = await caches.open(STRINGS_CACHE)
  await cache.put(STRINGS_KEY, new Response(JSON.stringify(clean)))
}

async function readStrings() {
  try {
    const hit = await (await caches.open(STRINGS_CACHE)).match(STRINGS_KEY)
    return hit ? await hit.json() : {}
  } catch {
    return {}
  }
}

function notificationId(value) {
  return Number.isSafeInteger(value) && value > 0 ? value : 0
}

self.addEventListener('push', (event) => {
  event.waitUntil(showPush(event))
})

async function showPush(event) {
  let data = {}
  try {
    data = event.data ? event.data.json() : {}
  } catch {
    data = {}
  }
  const id = notificationId(data?.id)
  const kind = KINDS.includes(data?.kind) ? data.kind : 'changed'
  const strings = await readStrings()
  await self.registration.showNotification(BRAND, {
    body: strings[kind] || strings.changed || '',
    tag: id ? `taskto-notification-${id}` : 'taskto-notification',
    data: { id },
    icon: '/icons/icon-192.png',
  })
  for (const client of await self.clients.matchAll({ type: 'window', includeUncontrolled: true })) {
    client.postMessage({ type: 'PUSH_RECEIVED' })
  }
}

self.addEventListener('notificationclick', (event) => {
  event.notification.close()
  const id = notificationId(event.notification.data?.id)
  event.waitUntil(openApp(id ? `/notification/${id}` : '/'))
})

async function openApp(path) {
  const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true })
  const open = windows.find((c) => new URL(c.url).origin === self.location.origin)
  if (open) {
    await open.focus()
    open.postMessage({ type: 'OPEN', path })
    return
  }
  await self.clients.openWindow(path)
}

self.addEventListener('fetch', (event) => {
  const request = event.request
  if (request.method !== 'GET') return
  const url = new URL(request.url)
  if (url.origin !== self.location.origin) return
  if (isServerPath(url.pathname)) return

  if (request.mode === 'navigate') {
    event.respondWith(navigation(request))
    return
  }
  if (PRECACHE.includes(url.pathname) || url.pathname.startsWith('/assets/')) {
    event.respondWith(fromCache(request, url.pathname))
  }
})

async function navigation(request) {
  try {
    return await fetch(request)
  } catch (e) {
    const shell = await lookup(SHELL)
    if (shell) return shell
    throw e
  }
}

async function fromCache(request, pathname) {
  return (await lookup(pathname)) || fetch(request)
}

async function lookup(pathname) {
  const own = await (await caches.open(CACHE)).match(pathname)
  return own || caches.match(pathname)
}
