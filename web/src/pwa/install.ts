import { useSyncExternalStore } from 'react'

interface InstallPromptEvent extends Event {
  prompt(): Promise<void>
  userChoice: Promise<{ outcome: 'accepted' | 'dismissed' }>
}

export type Platform = 'ios' | 'android' | 'desktop'

export interface InstallInfo {
  installed: boolean
  secure: boolean
  platform: Platform
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

export function listenForInstall() {
  window.addEventListener('beforeinstallprompt', (e) => {
    e.preventDefault()
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

export async function promptInstall(): Promise<'accepted' | 'dismissed' | 'unavailable'> {
  const event = deferred
  if (!event) return 'unavailable'
  deferred = null
  emit()
  await event.prompt()
  return (await event.userChoice).outcome
}

export function resetInstallInfo() {
  deferred = null
  installed = false
  emit()
}
