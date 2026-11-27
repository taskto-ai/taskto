import { useSyncExternalStore } from 'react'

const SW_URL = '/sw.js'
const CHECK_EVERY_MS = 30 * 60 * 1000

let ready = false
let dismissed = false
let asked = false
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
    if (asked) reload()
  })
}

export function registerServiceWorker() {
  if (!import.meta.env.PROD || !('serviceWorker' in navigator) || !window.isSecureContext) return
  const container = navigator.serviceWorker
  const start = async () => {
    let registration: ServiceWorkerRegistration
    try {
      registration = await container.register(SW_URL, { scope: '/', updateViaCache: 'none' })
    } catch {
      return
    }
    track(registration, container)
    let last = Date.now()
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState !== 'visible' || Date.now() - last < CHECK_EVERY_MS) return
      last = Date.now()
      registration.update().catch(() => {})
    })
  }
  if (document.readyState === 'complete') void start()
  else window.addEventListener('load', () => void start(), { once: true })
}

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

export function useUpdatePrompt(): boolean {
  return useSyncExternalStore(
    subscribe,
    () => ready && !dismissed,
    () => false,
  )
}

export function resetUpdateState() {
  ready = dismissed = asked = false
  waiting = null
  emit()
}
