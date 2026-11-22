// Installing the app on a device: what to tell this person, by platform (docs/pwa.md).
//
// Only Chromium offers a prompt a page can start (beforeinstallprompt); Safari on iOS has
// none, and the person adds the page to the Home Screen from the share sheet. So the page
// says what is true here, and offers a button only when the browser gave it one.

import { useSyncExternalStore } from 'react'

// The one non-standard event this uses, which TypeScript's DOM types don't have.
interface InstallPromptEvent extends Event {
  prompt(): Promise<void>
  userChoice: Promise<{ outcome: 'accepted' | 'dismissed' }>
}

export type Platform = 'ios' | 'android' | 'desktop'

export interface InstallInfo {
  /** Running as an installed app (a window of its own), not in a browser tab. */
  installed: boolean
  /** A secure context: HTTPS, or localhost. Without one there is no install and no offline. */
  secure: boolean
  platform: Platform
  /** The browser gave a prompt to start; a button can use it. */
  canPrompt: boolean
}

let deferred: InstallPromptEvent | null = null
let installed = false
const listeners = new Set<() => void>()
let cached: InstallInfo | null = null

function emit() {
  cached = null
  for (const l of listeners) l()
}

// Called once at start-up: the browser may fire the event before any screen that wants it is
// drawn, so it is kept here.
export function listenForInstall() {
  window.addEventListener('beforeinstallprompt', (e) => {
    e.preventDefault() // the page offers it when it chooses, not the browser's mini-bar
    deferred = e as InstallPromptEvent
    emit()
  })
  window.addEventListener('appinstalled', () => {
    deferred = null
    installed = true
    emit()
  })
}

export function platformOf(ua: string, platform: string, touchPoints: number): Platform {
  // An iPad asks for the desktop site and says it is a Mac: touch points tell them apart.
  if (/iPhone|iPad|iPod/.test(ua) || (platform === 'MacIntel' && touchPoints > 1)) return 'ios'
  if (/Android/.test(ua)) return 'android'
  return 'desktop'
}

function isStandalone(): boolean {
  const nav = navigator as Navigator & { standalone?: boolean }
  return window.matchMedia?.('(display-mode: standalone)').matches === true || nav.standalone === true
}

function read(): InstallInfo {
  cached ??= {
    installed: installed || isStandalone(),
    secure: window.isSecureContext,
    platform: platformOf(navigator.userAgent, navigator.platform, navigator.maxTouchPoints),
    canPrompt: deferred !== null,
  }
  return cached
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export function useInstall(): InstallInfo {
  return useSyncExternalStore(subscribe, read, read)
}

/** Starts the browser's own install prompt, when it gave one. */
export async function promptInstall(): Promise<'accepted' | 'dismissed' | 'unavailable'> {
  const event = deferred
  if (!event) return 'unavailable'
  deferred = null // a prompt can be used once
  emit()
  await event.prompt()
  return (await event.userChoice).outcome
}

// For tests, which change what the browser says between one test and the next.
export function resetInstallInfo() {
  deferred = null
  installed = false
  emit()
}
