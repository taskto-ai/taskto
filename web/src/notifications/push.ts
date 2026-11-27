import { api, forAccount } from '../api'
import type { InstallInfo } from '../pwa/install'
import { SessionEnded, sameSession, sessionUser, steps } from '../session'

export type PushState = 'unsupported' | 'ios-install' | 'denied' | 'off' | 'on'

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
  } catch {}
}

const PUSH_LOCK_WAIT_MS = 10_000

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
      return Promise.resolve().then(work).then(resolve, reject)
    }
    chain = chain.then(turn, turn).catch(() => {})
  })
}

const UNSUBSCRIBING = 'taskto.push-unsubscribing'
const UNSUBSCRIBE_UNRESOLVED_MS = 60_000

function unsubscribing(): { id: string; at: number }[] {
  try {
    const list = JSON.parse(localStorage.getItem(UNSUBSCRIBING) ?? '[]') as unknown
    return Array.isArray(list)
      ? list.filter(
          (e): e is { id: string; at: number } =>
            !!e &&
            typeof e.id === 'string' &&
            typeof e.at === 'number' &&
            Date.now() - e.at < UNSUBSCRIBE_UNRESOLVED_MS,
        )
      : []
  } catch {
    return []
  }
}

function setUnsubscribing(list: { id: string; at: number }[]) {
  try {
    if (list.length === 0) localStorage.removeItem(UNSUBSCRIBING)
    else localStorage.setItem(UNSUBSCRIBING, JSON.stringify(list))
  } catch {}
}

function unsubscribeUnresolved(): boolean {
  return unsubscribing().length > 0
}

async function unsubscribe(sub: PushSubscription): Promise<boolean> {
  const id = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`
  setUnsubscribing([...unsubscribing(), { id, at: Date.now() }])
  try {
    return await sub.unsubscribe()
  } finally {
    setUnsubscribing(unsubscribing().filter((e) => e.id !== id))
  }
}

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

export async function pushState(install: Pick<InstallInfo, 'platform' | 'installed'>): Promise<PushState> {
  if (install.platform === 'ios' && !install.installed) return 'ios-install'
  if (!pushSupported()) return 'unsupported'
  if (Notification.permission === 'denied') return 'denied'
  const reg = await registration()
  if (!reg) return 'unsupported'
  const sub = await reg.pushManager.getSubscription()
  return sub && Notification.permission === 'granted' && read() !== null ? 'on' : 'off'
}

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

function subscriptionBody(sub: PushSubscription) {
  const json = sub.toJSON()
  return { endpoint: sub.endpoint, keys: { p256dh: json.keys?.p256dh ?? '', auth: json.keys?.auth ?? '' } }
}

export function enablePush(userId: number): Promise<void> {
  const s = steps()
  if (sessionUser() !== userId) return Promise.reject(new SessionEnded())
  return withPushLock(() => {
    s.check(undefined)
    return enableNow(userId, s)
  })
}

async function enableNow(userId: number, s: ReturnType<typeof steps>): Promise<void> {
  if (unsubscribeUnresolved()) throw new PushBusy()
  const reg = s.check(await registration())
  if (!reg) throw new Error('push unavailable')
  const permission = s.check(Notification.permission === 'granted' ? 'granted' : await Notification.requestPermission())
  if (permission !== 'granted') throw new PushDenied()
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
  if (sub && !sameKey(sub.options.applicationServerKey, key)) {
    await unsubscribe(sub)
    s.check(undefined)
    sub = null
  }
  const fresh = sub === null
  sub ??= await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key })
  try {
    s.check(undefined)
    if (unsubscribeUnresolved()) throw new PushBusy()
    await api(SUBSCRIPTION_PATH, { method: 'PUT', body: subscriptionBody(sub), headers: forAccount(userId) })
    s.check(undefined)
  } catch (e) {
    if (fresh && !someoneElses(userId)) await unsubscribe(sub).catch(() => {})
    throw e
  }
  write(String(userId))
}

function someoneElses(userId: number): boolean {
  const signedIn = sessionUser()
  const flag = read()
  return (signedIn !== null && signedIn !== userId) || (flag !== null && flag !== String(userId))
}

export class PushUnsubscribeFailed extends Error {}

export function disablePush(): Promise<void> {
  const s = steps()
  const who = sessionUser()
  if (who === null) return Promise.reject(new SessionEnded())
  return withPushLock(() => {
    s.check(undefined)
    return disableNow(who, s)
  })
}

async function disableNow(who: number, s: ReturnType<typeof steps>): Promise<void> {
  const before = read()
  const sub = s.check(await (await registration())?.pushManager.getSubscription())
  if (sub) {
    await api(SUBSCRIPTION_PATH, { method: 'DELETE', body: { endpoint: sub.endpoint }, headers: forAccount(who) })
    s.check(undefined)
    const now = read()
    if (now !== null && now !== String(who) && now !== before) return
    if (!(await unsubscribe(sub).catch(() => false))) throw new PushUnsubscribeFailed()
    s.check(undefined)
  }
  write(null)
}

export async function leavePush(userId: number): Promise<void> {
  if (!pushSupported()) return
  const mine = sameSession()
  const theirs = () => {
    const flag = read()
    return flag !== null && flag !== String(userId)
  }
  if (!theirs()) write(null)
  const leave = () =>
    withPushLock(async () => {
      if (!theirs() && mine()) write(null)
      const sub = await (await registration())?.pushManager.getSubscription()
      if (!sub || !mine() || theirs()) return
      await unsubscribe(sub).catch(() => {})
    })
  await Promise.race([leave().catch(() => {}), new Promise((done) => setTimeout(done, 3000))])
}

export function syncPush(userId: number, inWorkspace = true): Promise<boolean> {
  if (!inWorkspace || read() !== String(userId) || !pushSupported()) return Promise.resolve(false)
  return withPushLock(() => syncNow(userId))
}

async function syncNow(userId: number): Promise<boolean> {
  if (read() !== String(userId)) return false
  if (unsubscribeUnresolved()) return false
  const s = steps()
  const reg = await registration()
  const sub = await reg?.pushManager.getSubscription()
  if (!s.ok()) return false
  if (!sub || Notification.permission !== 'granted') {
    write(null)
    return false
  }
  await api(SUBSCRIPTION_PATH, { method: 'PUT', body: subscriptionBody(sub), headers: forAccount(userId) }).catch(
    () => {},
  )
  return s.ok()
}

export async function sendPushStrings(strings: Record<string, string>): Promise<void> {
  const s = steps()
  const reg = await registration()
  if (s.ok()) reg?.active?.postMessage({ type: 'PUSH_STRINGS', strings })
}
