// TaskTo.AI's service worker. The build (pwa/plugin.ts) fills in VERSION and PRECACHE and
// writes it to the app's root as /sw.js; docs/pwa.md explains the design.
//
// What it does, and only this:
//
// - installs by downloading the whole static app into one versioned cache: the page, the
//   manifest, the icons and every script and stylesheet the build made, the lazy-loaded
//   ones (the apps renderer) included. The install fails, and the old worker stays, when any
//   of them can't be downloaded, so an installed worker always has a complete app and the
//   app is "ready offline" only when it does;
// - answers a request for one of those files (and any other under /assets/, for a page of
//   the previous version) from that cache, and goes to the network for what it lacks;
// - answers a page navigation from the network, and from the cached page only when the
//   network fails, so a link to an app route opens offline while the server's own routes
//   (/api, /mcp, /healthz) are never answered by the worker;
// - waits: a new version installs but doesn't take over until the page asks it to (the
//   update prompt, src/pwa), so an update never reloads a page with unsaved work.
//
// It never stores or answers anything from /api or any other request the server answers with
// a person's data: it answers only static files under the app's own names, and page
// navigations; and it stores only at install.

const VERSION = '__VERSION__'
const PRECACHE = /* __PRECACHE__ */ []

const PREFIX = 'taskto-shell-'
const CACHE = PREFIX + VERSION
// The page, kept under one key whatever app route it was fetched for.
const SHELL = '/index.html'
// The server's own routes: a navigation to one is the server's to answer, even offline.
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
    // Straight from the network, past the browser's HTTP cache, so a file the server has
    // changed under the same name isn't stored in its old form.
    await Promise.all(
      PRECACHE.map(async (url) => {
        const response = await fetch(new Request(url, { cache: 'reload', credentials: 'same-origin' }))
        if (!response.ok) throw new Error(`${url}: ${response.status}`)
        await cache.put(url, response)
      }),
    )
  } catch (e) {
    // Not a whole app: no half cache is left behind, and the install fails.
    await caches.delete(CACHE)
    throw e
  }
}

self.addEventListener('activate', (event) => {
  event.waitUntil(cleanUp())
})

// Marks a cache whose version has been active: only such a version can have pages open.
const ACTIVATED = '/__taskto-activated'

// Keeps this version's cache and that of the last version that was active before it.
// Another tab may still run that version's page, which loads its lazy chunks by their old
// names: they stay answerable. A version that installed but never took over (a second deploy
// came before anyone updated) has no page open anywhere, and its cache goes; keeping it in
// place of the active one would break the open tabs' lazy chunks.
async function cleanUp() {
  await (await caches.open(CACHE)).put(ACTIVATED, new Response(''))
  const ours = (await caches.keys()).filter((k) => k.startsWith(PREFIX))
  // Caches come back in the order they were created: the last activated one before ours.
  let previous = null
  for (const k of ours) {
    if (k !== CACHE && (await (await caches.open(k)).match(ACTIVATED))) previous = k
  }
  await Promise.all(ours.filter((k) => k !== CACHE && k !== previous).map((k) => caches.delete(k)))
  // The first install takes over the page that registered it, so it works offline at once.
  // An update never gets here before the page asked for it (see "message").
  await self.clients.claim()
}

self.addEventListener('message', (event) => {
  if (event.data && event.data.type === 'SKIP_WAITING') self.skipWaiting()
})

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
  // The precached files, and any other hashed asset: a page of the previous version still
  // open asks for chunks this version doesn't have, which the previous cache still does.
  if (PRECACHE.includes(url.pathname) || url.pathname.startsWith('/assets/')) {
    event.respondWith(fromCache(request, url.pathname))
  }
})

// Network first: the page is never older than the server's. The cached one answers only when
// the network fails (a TypeError), not when the server answers with an error.
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

// This version's copy first; then the previous version's, for a page of that version that
// is still open.
async function lookup(pathname) {
  const own = await (await caches.open(CACHE)).match(pathname)
  return own || caches.match(pathname)
}
