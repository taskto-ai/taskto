// The service worker's registration and its updates (docs/pwa.md).
//
// A new version of the app installs in the background and then waits. Nothing reloads the
// page: the page shows a prompt (UpdateBanner), and only the person's choice to update asks
// the waiting worker to take over (a SKIP_WAITING message) and reloads the page once it has.
// So an update never replaces a page with unsaved work in it.

import { useSyncExternalStore } from 'react'

const SW_URL = '/sw.js'
// An open installed app is checked for a new version when it comes back to the foreground,
// at most this often. (The browser also checks on its own at each navigation.)
const CHECK_EVERY_MS = 30 * 60 * 1000

let ready = false // a new version is installed and waiting
let dismissed = false // "Later", for this page's life
let asked = false // the person chose to update: the next controller change reloads
const listeners = new Set<() => void>()
let waiting: ServiceWorker | null = null

function emit() {
  for (const l of listeners) l()
}

function setWaiting(worker: ServiceWorker | null) {
  waiting = worker
  ready = worker !== null
  if (worker) dismissed = false
  emit()
}

// The pieces of the browser's API the tracking uses, so a test can fake them.
export interface Container {
  controller: ServiceWorker | null
  addEventListener(type: 'controllerchange', listener: () => void): void
}
export interface Registration {
  waiting: ServiceWorker | null
  installing: ServiceWorker | null
  addEventListener(type: 'updatefound', listener: () => void): void
  update(): Promise<unknown>
}

// Follows a registration: a worker that finishes installing while another one controls the
// page is an update, and waits for the person's go-ahead. (With no controller it is the first
// install: nothing to prompt for, it takes over by itself.)
export function track(registration: Registration, container: Container, reload: () => void = () => location.reload()) {
  const consider = (worker: ServiceWorker | null) => {
    if (worker && container.controller) setWaiting(worker)
  }
  consider(registration.waiting)
  registration.addEventListener('updatefound', () => {
    const worker = registration.installing
    if (!worker) return
    worker.addEventListener('statechange', () => {
      if (worker.state === 'installed') consider(worker)
    })
  })
  container.addEventListener('controllerchange', () => {
    setWaiting(null)
    // Only the page that asked reloads. Another open tab keeps running as it is (the worker
    // keeps the previous version's files for it) until its person reloads.
    if (asked) reload()
  })
}

export function registerServiceWorker() {
  // A service worker needs a secure context (HTTPS, or localhost); development (Vite) serves
  // no worker file, and a stale one there would hide changes.
  if (!import.meta.env.PROD || !('serviceWorker' in navigator) || !window.isSecureContext) return
  const container = navigator.serviceWorker
  const start = async () => {
    let registration: ServiceWorkerRegistration
    try {
      registration = await container.register(SW_URL, { scope: '/', updateViaCache: 'none' })
    } catch {
      return // the app works without it
    }
    track(registration, container)
    let last = Date.now()
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState !== 'visible' || Date.now() - last < CHECK_EVERY_MS) return
      last = Date.now()
      registration.update().catch(() => {}) // offline: next time
    })
  }
  // After the page has loaded, so installing (a download of the whole app) never competes with it.
  if (document.readyState === 'complete') void start()
  else window.addEventListener('load', () => void start(), { once: true })
}

// The person's choice to update now: the waiting worker takes over and the page reloads.
export function applyUpdate() {
  if (!waiting) return
  asked = true
  waiting.postMessage({ type: 'SKIP_WAITING' })
}

export function dismissUpdate() {
  dismissed = true
  emit()
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

// Whether to prompt: a new version is waiting, and the person hasn't said "Later".
export function useUpdatePrompt(): boolean {
  return useSyncExternalStore(
    subscribe,
    () => ready && !dismissed,
    () => false,
  )
}

// For tests.
export function resetUpdateState() {
  ready = dismissed = asked = false
  waiting = null
  emit()
}
