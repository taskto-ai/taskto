// Web Push in this browser: optional, explicit, and never needed (docs/pwa.md#notifications).
//
// Nothing here runs a subscription until the person turns push on in the settings, with a click:
// the permission prompt is theirs to answer then, not on load. A browser that can't push (no
// PushManager, no Notification, no secure context, iOS outside the installed app), or whose person
// said no, is told so and keeps the inbox, which is the record: a push is a prompt to look at it,
// it may arrive late or not at all, and nothing here syncs anything.
//
// One browser has at most one subscription. This module keeps a small flag in localStorage that
// says which person turned it on, so a browser doesn't hand its push endpoint to whoever signs in
// next: the subscription is removed when the person signs out (the server removes it with the
// session as well), and re-registered at the next start only for the same person.

import { api } from '../api'
import type { InstallInfo } from '../pwa/install'

export type PushState =
  /** This browser can't push at all. */
  | 'unsupported'
  /** iOS only pushes to an app installed on the Home Screen. */
  | 'ios-install'
  /** The person (or their browser's settings) said no. */
  | 'denied'
  | 'off'
  | 'on'

const FLAG = 'taskto.push'
const KEY_PATH = '/api/me/push/key'
const SUBSCRIPTION_PATH = '/api/me/push/subscription'

function read(): string | null {
  try {
    return localStorage.getItem(FLAG)
  } catch {
    return null
  }
}

function write(value: string | null) {
  try {
    if (value === null) localStorage.removeItem(FLAG)
    else localStorage.setItem(FLAG, value)
  } catch {
    // Not remembered: push is registered again only when turned on again.
  }
}

/** Whether this browser has what push needs: feature-detected, never from the user agent. */
export function pushSupported(): boolean {
  return (
    typeof window !== 'undefined' &&
    window.isSecureContext &&
    'serviceWorker' in navigator &&
    'PushManager' in window &&
    'Notification' in window
  )
}

async function registration(): Promise<ServiceWorkerRegistration | undefined> {
  if (!pushSupported()) return undefined
  try {
    return await navigator.serviceWorker.getRegistration()
  } catch {
    return undefined
  }
}

/** What to tell this person about push on this device, and whether it is on. */
export async function pushState(install: Pick<InstallInfo, 'platform' | 'installed'>): Promise<PushState> {
  if (install.platform === 'ios' && !install.installed) return 'ios-install'
  if (!pushSupported()) return 'unsupported'
  if (Notification.permission === 'denied') return 'denied'
  const reg = await registration()
  if (!reg) return 'unsupported' // no worker (a development server, a failed registration): nothing shows a push
  const sub = await reg.pushManager.getSubscription()
  return sub && Notification.permission === 'granted' && read() !== null ? 'on' : 'off'
}

/** Thrown by enablePush when the person said no. */
export class PushDenied extends Error {}

function keyBytes(base64url: string): Uint8Array<ArrayBuffer> {
  const pad = '='.repeat((4 - (base64url.length % 4)) % 4)
  const raw = atob((base64url + pad).replace(/-/g, '+').replace(/_/g, '/'))
  return Uint8Array.from(raw, (c) => c.charCodeAt(0))
}

function sameKey(a: ArrayBuffer | null, b: Uint8Array): boolean {
  if (!a) return false
  const x = new Uint8Array(a)
  return x.length === b.length && x.every((v, i) => v === b[i])
}

// What the server takes of a PushSubscription: its endpoint and keys, and nothing else (toJSON()
// adds an expiration the API doesn't know).
function subscriptionBody(sub: PushSubscription) {
  const json = sub.toJSON()
  return { endpoint: sub.endpoint, keys: { p256dh: json.keys?.p256dh ?? '', auth: json.keys?.auth ?? '' } }
}

/** Turns push on for this person on this device. Call it from a click. */
export async function enablePush(userId: number): Promise<void> {
  const reg = await registration()
  if (!reg) throw new Error('push unavailable')
  const permission = Notification.permission === 'granted' ? 'granted' : await Notification.requestPermission()
  if (permission !== 'granted') throw new PushDenied()
  const { public_key } = await api<{ public_key: string }>(KEY_PATH)
  const key = keyBytes(public_key)
  let sub = await reg.pushManager.getSubscription()
  // A subscription made for another server key is of no use: made again.
  if (sub && !sameKey(sub.options.applicationServerKey, key)) {
    await sub.unsubscribe()
    sub = null
  }
  const fresh = sub === null
  sub ??= await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key })
  try {
    await api(SUBSCRIPTION_PATH, { method: 'PUT', body: subscriptionBody(sub) })
  } catch (e) {
    // The server doesn't know this device: the browser mustn't keep a subscription it never got.
    if (fresh) await sub.unsubscribe().catch(() => {})
    throw e
  }
  write(String(userId))
}

/** Thrown by disablePush when the browser wouldn't end its subscription. */
export class PushUnsubscribeFailed extends Error {}

/**
 * Turns push off for this device: the server forgets it, then the browser does. It says what
 * went wrong and leaves this device as it was, so that what Settings shows stays true and the
 * person can try again: nothing is remembered as off until both are done (the server's answer
 * to a delete of a device it doesn't know is the same as to one it does).
 */
export async function disablePush(): Promise<void> {
  const sub = await (await registration())?.pushManager.getSubscription()
  if (sub) {
    await api(SUBSCRIPTION_PATH, { method: 'DELETE', body: { endpoint: sub.endpoint } })
    if (!(await sub.unsubscribe().catch(() => false))) throw new PushUnsubscribeFailed()
  }
  write(null)
}

/**
 * Called once the person's session has ended (the server confirmed the sign-out, or said the
 * session is gone): this device stops receiving what is theirs. The server has already removed the
 * session's subscription with it, so only the browser's is left to end. Best effort and bounded: a
 * push service that is told the subscription is over answers "gone" to the next one.
 */
export async function leavePush(): Promise<void> {
  if (!pushSupported()) return
  write(null)
  const leave = async () => {
    const sub = await (await registration())?.pushManager.getSubscription()
    if (!sub) return
    await sub.unsubscribe().catch(() => {})
  }
  // Bounded: signing out never waits long on a browser that is slow to answer.
  await Promise.race([leave().catch(() => {}), new Promise((done) => setTimeout(done, 3000))])
}

/**
 * Called when the app starts signed in: a device whose person turned push on is registered again
 * (a new login is a new session, and a browser can change its endpoint). A device that was
 * turned on by another person, that lost its permission or its subscription, is left alone and
 * forgotten here, never registered for this person. Says whether this device is registered.
 * Nothing is registered for a person who is in no workspace: push is for people in one, and the
 * server refuses it.
 */
export async function syncPush(userId: number, inWorkspace = true): Promise<boolean> {
  if (!inWorkspace || read() !== String(userId) || !pushSupported()) return false
  const reg = await registration()
  const sub = await reg?.pushManager.getSubscription()
  if (!sub || Notification.permission !== 'granted') {
    write(null)
    return false
  }
  await api(SUBSCRIPTION_PATH, { method: 'PUT', body: subscriptionBody(sub) }).catch(() => {})
  return true
}

/** What the service worker says for each kind of push, in the person's language. */
export async function sendPushStrings(strings: Record<string, string>): Promise<void> {
  const reg = await registration()
  reg?.active?.postMessage({ type: 'PUSH_STRINGS', strings })
}
