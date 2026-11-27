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
// session as well), and re-registered at the next start only for the same person. While that person
// turns it off, the flag says so (`off:<id>`) from before the server is asked: a tab closed in the
// middle then leaves push shown off, and never registered again, once the server has forgotten
// the device.
//
// Every step here waits (the worker, the browser's prompt, the push service, the server), and the
// person can sign out, or someone else sign in, meanwhile. So each request names the account it is
// for (the server refuses it as another's), and after each wait the session is checked
// (session.ts): one that ended stops there, undoing a subscription it had just made, and never
// writes who turned push on, or another person's words, into this browser.

import { api, forAccount } from '../api'
import type { InstallInfo } from '../pwa/install'
import { SessionEnded, sameSession, sessionUser, steps } from '../session'

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

const TURNING_OFF = 'off:'

// Whose the flag is: the person who turned push on, or who is turning it off. A turn-off's marker is
// its own (`off:<id>:<turn>:<base>`) and carries the flag as it was before any turn-off began
// (`base`): one that fails puts that back, and only while the marker is still its own, so it never
// undoes another tab's turn-off, nor brings back the marker of one that is over.
const ownerOf = (flag: string | null) =>
  flag?.startsWith(TURNING_OFF) ? flag.slice(TURNING_OFF.length).split(':')[0] : flag
const baseOf = (flag: string | null): string | null =>
  flag?.startsWith(TURNING_OFF) ? flag.slice(TURNING_OFF.length).split(':')[2] || null : flag

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

// The browser has one push subscription, shared by its tabs. Every change to it (turning push on or
// off, a sign-out's removal, registering again at start) goes one at a time across the tabs (Web
// Locks), so no tab ends a subscription another is in the middle of registering; where the browser
// has no Web Locks, one at a time in this tab.
//
// Waiting for that turn is bounded (PushBusy), but a turn is never given up while the browser is
// still changing the subscription: the lock is held until the work is over, however long it takes,
// so the next change never starts on a subscription that is still being ended.
//
// Across tabs without Web Locks, nothing orders the changes, so one rule stands in: a subscription is
// never reused (turned on, or registered again) while an unsubscribe of it, in any tab, is not
// resolved (`UNSUBSCRIBING`, below). The person is told to try again in a moment.
const PUSH_LOCK_WAIT_MS = 10_000

/** Thrown when push can't be changed now: another change of it, here or in another tab, is going on. */
export class PushBusy extends Error {}

let chain: Promise<unknown> = Promise.resolve()
function withPushLock<T>(work: () => Promise<T>): Promise<T> {
  const locks = typeof navigator !== 'undefined' ? navigator.locks : undefined
  if (locks?.request) {
    let granted = false
    const signal = typeof AbortSignal.timeout === 'function' ? AbortSignal.timeout(PUSH_LOCK_WAIT_MS) : undefined
    return (
      locks.request('taskto-push-subscription', signal ? { signal } : {}, () => {
        granted = true
        return work()
      }) as Promise<T>
    ).catch((e) => {
      // Only the wait timed out, never the work: once granted, it runs to its end.
      if (!granted && signal?.aborted) throw new PushBusy()
      throw e
    })
  }
  return new Promise<T>((resolve, reject) => {
    let state: 'waiting' | 'running' | 'gave-up' = 'waiting'
    const timer = setTimeout(() => {
      if (state !== 'waiting') return
      state = 'gave-up'
      reject(new PushBusy())
    }, PUSH_LOCK_WAIT_MS)
    const turn = () => {
      clearTimeout(timer)
      if (state === 'gave-up') return
      state = 'running'
      // The chain waits for the work itself, so the next change starts only once this one is over.
      // (Work that throws at once is a failure like any other.)
      return Promise.resolve().then(work).then(resolve, reject)
    }
    chain = chain.then(turn, turn).catch(() => {})
  })
}

// The unsubscribes of this browser's subscription that are going on, in any tab: each one's own
// key (`UNSUBSCRIBING:<id>`, holding when it began), written before it starts and removed when the
// browser has answered. A key of its own, so no tab ever writes back another's mark from what it
// read before (two tabs at once, without Web Locks to order them). A mark is taken to be over after a
// while (UNSUBSCRIBE_UNRESOLVED_MS), so a tab closed in the middle doesn't hold push off for good.
const UNSUBSCRIBING = 'taskto.push-unsubscribing:'
const UNSUBSCRIBE_UNRESOLVED_MS = 60_000

// The turn-offs going on, in any tab: each one's own mark, the same way (a turn-off that fails puts
// push back only when no other is still on its way).
const TURNING_OFF_MARK = 'taskto.push-turning-off:'

// Whether an unsubscribe of the browser's subscription isn't resolved yet, in this tab or another.
const unsubscribeUnresolved = () => freshMarks(UNSUBSCRIBING)

// Whether a mark under `prefix` is still fresh. Marks that are over (a tab closed in the middle) or
// that hold no time are removed as they are found.
function freshMarks(prefix: string): boolean {
  let unresolved = false
  try {
    const over: string[] = []
    for (let i = 0; i < localStorage.length; i++) {
      const key = localStorage.key(i)
      if (!key?.startsWith(prefix)) continue
      const raw = localStorage.getItem(key)
      const at = raw === null || raw.trim() === '' ? Number.NaN : Number(raw)
      if (Number.isFinite(at) && Math.abs(Date.now() - at) < UNSUBSCRIBE_UNRESOLVED_MS) unresolved = true
      else over.push(key)
    }
    for (const key of over) localStorage.removeItem(key)
  } catch {
    // Nothing kept: where storage is refused, Web Locks (or this tab's chain) still order the changes.
  }
  return unresolved
}

function mark(key: string, on: boolean) {
  try {
    if (on) localStorage.setItem(key, String(Date.now()))
    else localStorage.removeItem(key)
  } catch {
    // Not kept: as above.
  }
}

// Ends the subscription, marked as going on until the browser answers. True when it ended.
async function unsubscribe(sub: PushSubscription): Promise<boolean> {
  const key = `${UNSUBSCRIBING}${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`
  mark(key, true)
  try {
    return await sub.unsubscribe()
  } finally {
    mark(key, false)
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
  const flag = read()
  return sub && Notification.permission === 'granted' && flag !== null && !flag.startsWith(TURNING_OFF) ? 'on' : 'off'
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

/**
 * Turns push on for this person on this device. Call it from a click: the session is the one of the
 * click, taken before the turn is waited for, and checked again when it comes, so a turn that comes
 * after the person signed out (or someone else in) does nothing.
 */
export function enablePush(userId: number): Promise<void> {
  const s = steps()
  if (sessionUser() !== userId) return Promise.reject(new SessionEnded())
  return withPushLock(() => {
    s.check(undefined)
    return enableNow(userId, s)
  })
}

async function enableNow(userId: number, s: ReturnType<typeof steps>): Promise<void> {
  // A subscription that is being ended (in another tab) is not taken up again.
  if (unsubscribeUnresolved()) throw new PushBusy()
  const reg = s.check(await registration())
  if (!reg) throw new Error('push unavailable')
  const permission = s.check(Notification.permission === 'granted' ? 'granted' : await Notification.requestPermission())
  if (permission !== 'granted') throw new PushDenied()
  // The browser's subscription is claimed for this account before it is looked at or reused: a
  // cleanup of another's (a sign-out, a failed turning on) then leaves it alone. Given back if
  // turning push on doesn't finish.
  const before = read()
  write(String(userId))
  try {
    await registerFor(userId, reg, s)
  } catch (e) {
    if (read() === String(userId) && before !== String(userId)) write(before)
    throw e
  }
}

async function registerFor(userId: number, reg: ServiceWorkerRegistration, s: ReturnType<typeof steps>): Promise<void> {
  const { public_key } = await api<{ public_key: string }>(KEY_PATH)
  const key = keyBytes(public_key)
  s.check(undefined)
  let sub = s.check(await reg.pushManager.getSubscription())
  // A subscription made for another server key is of no use: made again.
  if (sub && !sameKey(sub.options.applicationServerKey, key)) {
    await unsubscribe(sub)
    s.check(undefined)
    sub = null
  }
  const fresh = sub === null
  sub ??= await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key })
  try {
    s.check(undefined)
    // Asked again before it is handed to the server: another tab may have begun ending it.
    if (unsubscribeUnresolved()) throw new PushBusy()
    await api(SUBSCRIPTION_PATH, { method: 'PUT', body: subscriptionBody(sub), headers: forAccount(userId) })
    s.check(undefined)
  } catch (e) {
    // The server doesn't know this device, or the session ended meanwhile: the browser mustn't keep
    // a subscription that isn't the signed-in person's. Unless it is someone else's by now: the
    // browser's subscription is one, shared by its tabs, and another may have signed someone in who
    // turned push on with it meanwhile. Then it is theirs, and left alone.
    if (fresh && !someoneElses(userId)) await unsubscribe(sub).catch(() => {})
    throw e
  }
  write(String(userId))
}

// Whether this browser's push is someone else's than `userId` now: another account's session has
// begun here, or the flag names another account. Asked at the moment a subscription would be ended.
function someoneElses(userId: number): boolean {
  const signedIn = sessionUser()
  const flag = ownerOf(read())
  return (signedIn !== null && signedIn !== userId) || (flag !== null && flag !== String(userId))
}

/** Thrown by disablePush when the browser wouldn't end its subscription. */
export class PushUnsubscribeFailed extends Error {}

/**
 * Turns push off for this device: the server forgets it, then the browser does. It says what
 * went wrong and leaves this device as it was, so that what Settings shows stays true and the
 * person can try again: nothing is remembered as off until both are done (the server's answer
 * to a delete of a device it doesn't know is the same as to one it does).
 */
export function disablePush(): Promise<void> {
  // The session of the click, as for turning it on.
  const s = steps()
  const who = sessionUser()
  if (who === null) return Promise.reject(new SessionEnded())
  return withPushLock(() => {
    s.check(undefined)
    return disableNow(who, s)
  })
}

async function disableNow(who: number, s: ReturnType<typeof steps>): Promise<void> {
  // Who the flag named when the person asked: a flag that was another account's already (their
  // session ended with no tab to clean up) is what this person turns off, along with the rest.
  const before = read()
  const sub = s.check(await (await registration())?.pushManager.getSubscription())
  if (sub) {
    // Turning off from here on, for every tab and the next start, until it is done or given up.
    const turn = `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 8)}`
    // Scoped to the account: another account's turn-off, left by a tab that closed, decides nothing here.
    const pending = `${TURNING_OFF_MARK}${who}:${turn}`
    mark(pending, true)
    write(`${TURNING_OFF}${who}:${turn}:${baseOf(before) ?? ''}`)
    // This turn-off didn't happen: push is as it was before any turn-off began, unless another one is
    // still on its way (it decides), or someone has the flag now (turned on, or off, since).
    const giveUp = () => {
      mark(pending, false)
      const now = read()
      if (now?.startsWith(TURNING_OFF) && ownerOf(now) === String(who) && !freshMarks(`${TURNING_OFF_MARK}${who}:`))
        write(baseOf(before))
    }
    try {
      try {
        await api(SUBSCRIPTION_PATH, { method: 'DELETE', body: { endpoint: sub.endpoint }, headers: forAccount(who) })
      } catch (e) {
        // The server still knows the device.
        giveUp()
        throw e
      }
      s.check(undefined)
      // Asked at the moment it would be ended: another account (another tab, where the order of
      // changes isn't kept) may have claimed the browser's subscription while the server answered.
      // It is theirs then, with their flag; this account's turning off is done with the server's answer.
      const now = read()
      if (now !== null && ownerOf(now) !== String(who)) return
      if (!(await unsubscribe(sub).catch(() => false))) {
        // The browser kept it: shown as it was, for the person to try again.
        giveUp()
        throw new PushUnsubscribeFailed()
      }
      s.check(undefined)
    } finally {
      mark(pending, false)
    }
  }
  // Asked again after the waits: a flag another account wrote meanwhile (where nothing orders the
  // changes) is theirs, and stays.
  const now = read()
  if (now === null || ownerOf(now) === String(who)) write(null)
}

/**
 * Called once the person's session has ended (the server confirmed the sign-out, or said the
 * session is gone): this device stops receiving what is theirs. The server has already removed the
 * session's subscription with it, so only the browser's is left to end. Best effort and bounded: a
 * push service that is told the subscription is over answers "gone" to the next one.
 */
//
// It is part of the sign-out of `userId`, whose session is already over here (session.ts): the
// request names them, so it can't remove the device of someone who signed in since. `still` says
// whether the sign-out is still the one of the browser's login: a newer login of the same account,
// started in another tab while this waits, has the subscription from then on.
export async function leavePush(userId: number, still: () => boolean = () => true): Promise<void> {
  if (!pushSupported()) return
  // The sign-out this is part of: once another session has begun here, or someone else turned push
  // on (the flag names them), or a newer login of the account started, this browser's subscription
  // is theirs, and is left alone. That is checked again at the moment it would be ended, since the
  // browser is shared by its tabs.
  const mine = sameSession()
  const ours = () => mine() && still()
  const theirs = () => {
    const flag = ownerOf(read())
    return flag !== null && flag !== String(userId)
  }
  if (!theirs() && still()) write(null)
  const leave = () =>
    withPushLock(async () => {
      // Its turn may come after another account's claim failed and gave the flag back to this one:
      // the sign-out has the last word on it, unless a session began here since.
      if (!theirs() && ours()) write(null)
      const sub = await (await registration())?.pushManager.getSubscription()
      if (!sub || !ours() || theirs()) return
      await unsubscribe(sub).catch(() => {})
    })
  // Bounded: signing out never waits long on a browser that is slow to answer. What is still on
  // its way after that checks, before it touches the subscription, that it is still this sign-out's.
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
export function syncPush(userId: number, inWorkspace = true): Promise<boolean> {
  if (!inWorkspace || read() !== String(userId) || !pushSupported()) return Promise.resolve(false)
  return withPushLock(() => syncNow(userId))
}

async function syncNow(userId: number): Promise<boolean> {
  if (read() !== String(userId)) return false
  // Not registered again while it is being ended in another tab: the next start does it.
  if (unsubscribeUnresolved()) return false
  const s = steps()
  const reg = await registration()
  const sub = await reg?.pushManager.getSubscription()
  // The session ended while the browser answered: this device is the next person's to decide.
  if (!s.ok()) return false
  if (!sub || Notification.permission !== 'granted') {
    // Only while the flag is still this person's (another tab may have changed it meanwhile).
    if (read() === String(userId)) write(null)
    return false
  }
  // Asked again after the browser answered: another tab (where nothing orders the changes) may have
  // begun turning push off meanwhile. Then it is not registered again here.
  if (read() !== String(userId) || unsubscribeUnresolved()) return false
  await api(SUBSCRIPTION_PATH, { method: 'PUT', body: subscriptionBody(sub), headers: forAccount(userId) }).catch(
    () => {},
  )
  return s.ok()
}

/** What the service worker says for each kind of push, in the person's language. */
export async function sendPushStrings(strings: Record<string, string>): Promise<void> {
  const s = steps()
  const reg = await registration()
  // Another person's words never reach the worker: only while the session that sent them lasts.
  if (s.ok()) reg?.active?.postMessage({ type: 'PUSH_STRINGS', strings })
}
