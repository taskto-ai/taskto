// Sending what was made offline (docs/offline.md, "Sending the queue").
//
// The queue is sent in the order it was made, one record at a time, as the signed-in user, with
// the session the browser holds *now*: nothing about who may do what is remembered from when the
// record was made, so the server decides again on every send. A record goes to the server in two
// steps. First its photos, each uploaded through the resumable upload routes (an upload that was
// interrupted, by the network or by closing the page, goes on from where the server is); the
// record is sent only once every photo is finalized, so a record never names media that isn't
// there. Then the action, with the item's own operation key: sending it again (the answer was
// lost) changes nothing on the server, so a record is made exactly once however often it is sent.
//
// What the answer means:
//   2xx                      synced: the server acknowledged the record (and its photos)
//   401                      the session ended: sending pauses until someone signs in
//   no answer, 5xx, 408, 425, 429
//                            transient: try again later, backing off, up to MAX_ATTEMPTS times,
//                            then it needs attention and the person says when to try again
//   403, 404                 access was lost (the space, the app, the right to add): the item
//                            needs attention, and if the app can no longer be opened at all the
//                            downloaded copy is removed
//   409 (archived, outdated) and 400/422 (the server refused it)
//                            needs attention: not retried until the person says so; the input is
//                            kept

import { ApiError, api, SESSION_LOST } from '../api'
import { UploadCancelled, uploadPhoto } from '../apps/photos'
import type { Attachment, AttentionReason, ItemError, OfflineStore, OutboxItem } from './store'

export const MAX_ATTEMPTS = 8
const BASE_DELAY = 2_000
const MAX_DELAY = 5 * 60_000

// The wait before attempt n+1 after n failures: doubling, capped, with a little jitter so many
// devices coming back together don't arrive together.
export function backoff(attempts: number, random: () => number = Math.random): number {
  const wait = Math.min(MAX_DELAY, BASE_DELAY * 2 ** Math.max(0, attempts - 1))
  return Math.round(wait * (0.85 + random() * 0.3))
}

type Outcome =
  | { kind: 'synced' }
  | { kind: 'paused' }
  | { kind: 'transient'; error: ItemError }
  | { kind: 'attention'; reason: AttentionReason; error: ItemError; revoked?: boolean }

export function describe(e: unknown): ItemError {
  if (e instanceof ApiError) return { status: e.status, code: e.code, params: e.params, message: e.message }
  return { message: e instanceof Error ? e.message : String(e) }
}

// What a failed send means; the item's attachments say whether a photo was the cause.
export function classify(e: unknown): Outcome {
  const error = describe(e)
  if (!(e instanceof ApiError)) return { kind: 'transient', error }
  const { status, code } = e
  if (status === 401) return { kind: 'paused' }
  if (status >= 500 || status === 408 || status === 425 || status === 429) return { kind: 'transient', error }
  if (code === 'app_attachment_bad' || code === 'app_upload_state') return { kind: 'attention', reason: 'photo', error }
  if (status === 403 || status === 404) {
    if (code === 'space_archived' || code === 'app_offline') return { kind: 'attention', reason: 'archived', error }
    return {
      kind: 'attention',
      reason: 'access',
      error,
      revoked: status === 404 || code === 'offline_downloads_disabled',
    }
  }
  if (code === 'app_offline' || code === 'space_archived') return { kind: 'attention', reason: 'archived', error }
  if (code === 'app_release_outdated' || code === 'app_not_published' || code === 'app_release_not_found') {
    return { kind: 'attention', reason: 'outdated', error }
  }
  return { kind: 'attention', reason: 'refused', error }
}

export type SyncStatus = {
  // Why nothing is being sent, if so.
  paused: null | 'session' | 'manual'
  running: boolean
  // The browser's own idea of the network: a hint, never proof.
  online: boolean
}

export type SyncDeps = {
  now?: () => number
  // Where the timer is: a test's own.
  setTimer?: (fn: () => void, ms: number) => unknown
  clearTimer?: (handle: unknown) => void
  upload?: typeof uploadPhoto
  request?: typeof api
  random?: () => number
  // Called when the server says the app can't be opened any more: its downloaded copy goes.
  onRevoked?: (item: OutboxItem, why: ItemError) => void | Promise<void>
}

export class SyncEngine {
  private listeners = new Set<() => void>()
  private timer: unknown
  private running = false
  private paused: SyncStatus['paused'] = null
  private stopped = false
  private readonly now: () => number
  private readonly upload: typeof uploadPhoto
  private readonly request: typeof api

  constructor(
    private readonly store: OfflineStore,
    private readonly deps: SyncDeps = {},
  ) {
    this.now = deps.now ?? Date.now
    this.upload = deps.upload ?? uploadPhoto
    this.request = deps.request ?? api
  }

  status(): SyncStatus {
    return {
      paused: this.paused,
      running: this.running,
      online: typeof navigator === 'undefined' || navigator.onLine !== false,
    }
  }

  subscribe(fn: () => void): () => void {
    this.listeners.add(fn)
    return () => this.listeners.delete(fn)
  }

  private emit() {
    for (const fn of [...this.listeners]) fn()
  }

  // Starts sending whenever there is something to send: now, when the network comes back, when the
  // page comes to the front, and when an item's wait is over.
  start(): () => void {
    this.stopped = false
    const kick = () => void this.run()
    window.addEventListener('online', kick)
    const visible = () => document.visibilityState === 'visible' && kick()
    document.addEventListener('visibilitychange', visible)
    const lost = () => this.pause('session')
    window.addEventListener(SESSION_LOST, lost)
    kick()
    return () => {
      this.stopped = true
      window.removeEventListener('online', kick)
      document.removeEventListener('visibilitychange', visible)
      window.removeEventListener(SESSION_LOST, lost)
      this.clear()
    }
  }

  pause(reason: 'session' | 'manual') {
    this.paused = reason
    this.clear()
    this.emit()
  }

  // Sending goes on: someone signed in again (the session paused for), or the person said so.
  resume() {
    this.paused = null
    this.emit()
    void this.run()
  }

  private clear() {
    if (this.timer !== undefined) (this.deps.clearTimer ?? clearTimeout)(this.timer as never)
    this.timer = undefined
  }

  private schedule(at: number) {
    if (this.stopped || this.paused) return
    this.clear()
    const ms = Math.max(0, at - this.now())
    this.timer = (this.deps.setTimer ?? setTimeout)(() => {
      this.timer = undefined
      void this.run()
    }, ms)
  }

  // "Try again now": an item that waits for its next attempt, or needs attention, is sent at once.
  async retry(id?: string): Promise<void> {
    for (const item of await this.store.listItems()) {
      if (id !== undefined && item.id !== id) continue
      if (item.state === 'queued' || item.state === 'attention') {
        await this.store.updateItem(item.id, (i) => ({
          ...i,
          state: 'queued',
          attempts: 0,
          nextAttemptAt: undefined,
          attention: undefined,
          error: undefined,
          // A photo that was refused is uploaded again from the start.
          attachments: i.attachments.map((a) =>
            a.state === 'uploaded' && !a.serverId ? { ...a, state: 'pending' } : a,
          ),
        }))
      }
    }
    if (this.paused === 'manual') this.paused = null
    await this.run()
  }

  // Holds an item as a local draft: it stays on this device and isn't sent until it is sent on purpose.
  async hold(id: string): Promise<void> {
    await this.store.updateItem(id, (i) =>
      i.state === 'synced' || i.state === 'syncing' ? undefined : { ...i, state: 'draft' },
    )
    this.emit()
  }

  async release(id: string): Promise<void> {
    await this.store.updateItem(id, (i) =>
      i.state === 'draft' ? { ...i, state: 'queued', attempts: 0, nextAttemptAt: undefined } : undefined,
    )
    await this.run()
  }

  // One pass over the queue. Only one runs at a time here, and, where the browser has Web Locks,
  // in all of this user's tabs: two tabs sending the same item would still make it once (the
  // operation key), but needlessly upload its photos twice.
  async run(): Promise<void> {
    if (this.running || this.stopped || this.paused) return
    if (typeof navigator !== 'undefined' && navigator.onLine === false) return
    this.running = true
    this.emit()
    try {
      const locks = typeof navigator !== 'undefined' ? navigator.locks : undefined
      if (locks) {
        await locks.request(`taskto-outbox-${this.store.user}`, { ifAvailable: true }, async (lock) => {
          if (lock) await this.pass()
        })
      } else {
        await this.pass()
      }
    } finally {
      this.running = false
      this.emit()
    }
    // Wake up when the first item that is waiting is due.
    try {
      const waiting = (await this.store.listItems()).filter(
        (i) => i.state === 'queued' && i.nextAttemptAt !== undefined,
      )
      if (waiting.length > 0) this.schedule(Math.min(...waiting.map((i) => i.nextAttemptAt as number)))
    } catch {
      // The database was closed (signing out) while this pass ran: nothing is waiting any more.
    }
  }

  private async pass(): Promise<void> {
    for (const item of await this.store.listItems()) {
      if (this.stopped || this.paused) return
      // A `syncing` item found here was being sent by a page that was closed: it is sent again.
      const sendable = item.state === 'queued' || item.state === 'syncing'
      if (!sendable || (item.nextAttemptAt !== undefined && item.nextAttemptAt > this.now())) continue
      const outcome = await this.send(item)
      if (outcome.kind === 'paused') {
        this.pause('session')
        return
      }
      // The network is down: the items behind this one would find the same.
      if (outcome.kind === 'transient') return
    }
  }

  private async send(start: OutboxItem): Promise<Outcome> {
    const id = start.id
    const claimed = await this.store.updateItem(id, (i) =>
      i.state === 'queued' || i.state === 'syncing' ? { ...i, state: 'syncing' } : undefined,
    )
    if (claimed?.state !== 'syncing') return { kind: 'synced' }
    try {
      // Photos first: the record is sent once every one of them is finalized on the server.
      for (const a of claimed.attachments) {
        const current = (await this.store.getItem(id))?.attachments.find((x) => x.localId === a.localId)
        if (!current || (current.state === 'uploaded' && current.serverId)) continue
        await this.uploadOne(claimed, current)
      }
      const fresh = await this.store.getItem(id)
      if (!fresh) return { kind: 'synced' }
      if (fresh.attachments.some((a) => !(a.state === 'uploaded' && a.serverId))) {
        throw new ApiError(0, 'a photo is not on the server', { code: 'app_attachment_bad' })
      }
      await this.request(`${fresh.base}/actions`, {
        method: 'POST',
        body: { ...withServerPhotos(fresh), release: fresh.release, operation: fresh.operation },
      })
      await this.store.updateItem(id, (i) => ({
        ...i,
        state: 'synced',
        syncedAt: new Date(this.now()).toISOString(),
        attempts: i.attempts + 1,
        nextAttemptAt: undefined,
        attention: undefined,
        error: undefined,
      }))
      await this.store.dropBlobs(id)
      return { kind: 'synced' }
    } catch (e) {
      if (e instanceof UploadCancelled) return { kind: 'transient', error: describe(e) }
      return this.failed(id, e)
    }
  }

  private async uploadOne(item: OutboxItem, a: Attachment): Promise<void> {
    const blob = await this.store.getBlob(a.blobId)
    if (!blob) throw new ApiError(0, 'the photo is no longer on this device', { code: 'app_attachment_bad' })
    await this.patchAttachment(item.id, a.localId, { state: 'uploading' })
    const done = await this.upload(item.base, a.field, blob, {
      resume: a.uploadId,
      onStarted: (uploadId) => void this.patchAttachment(item.id, a.localId, { uploadId }),
      retries: 2,
    })
    await this.patchAttachment(item.id, a.localId, { state: 'uploaded', serverId: done.id, uploadId: done.id })
  }

  private patchAttachment(id: string, localId: string, change: Partial<Attachment>) {
    return this.store.updateItem(id, (i) => ({
      ...i,
      attachments: i.attachments.map((a) => (a.localId === localId ? { ...a, ...change } : a)),
    }))
  }

  // Records what a failed send means for the item, and acts on a revocation.
  private async failed(id: string, e: unknown): Promise<Outcome> {
    let outcome = classify(e)
    // A 403 or 404 only means the app can't be reached *now*: it is confirmed by asking for the
    // app itself, which is answered for anyone who may open it.
    if (outcome.kind === 'attention' && outcome.reason === 'access') {
      const item = await this.store.getItem(id)
      if (item) {
        const revoked = await this.revoked(item)
        outcome = { ...outcome, revoked }
        if (revoked) await this.deps.onRevoked?.(item, outcome.error)
      }
    }
    await this.store.updateItem(id, (i) => {
      switch (outcome.kind) {
        case 'paused':
          // Waits for the next session: not a failed attempt.
          return { ...i, state: 'queued' }
        case 'transient': {
          const attempts = i.attempts + 1
          if (attempts >= MAX_ATTEMPTS) {
            return {
              ...i,
              state: 'attention',
              attention: 'gave_up',
              attempts,
              nextAttemptAt: undefined,
              error: outcome.error,
            }
          }
          return {
            ...i,
            state: 'queued',
            attempts,
            nextAttemptAt: this.now() + backoff(attempts, this.deps.random),
            error: outcome.error,
          }
        }
        case 'attention':
          return {
            ...i,
            state: 'attention',
            attention: outcome.reason,
            nextAttemptAt: undefined,
            error: outcome.error,
            // A photo the server no longer has is uploaded again when the person retries.
            attachments:
              outcome.reason === 'photo'
                ? i.attachments.map((a) => ({ ...a, state: 'pending', uploadId: undefined, serverId: undefined }))
                : i.attachments,
          }
        default:
          return undefined
      }
    })
    return outcome
  }

  private async revoked(item: OutboxItem): Promise<boolean> {
    try {
      await this.request(item.base)
      return false
    } catch (e) {
      return e instanceof ApiError && (e.status === 403 || e.status === 404)
    }
  }
}

// The action to send: the context's local photo ids replaced by the server's.
export function withServerPhotos(item: OutboxItem): OutboxItem['action'] {
  const server = new Map(item.attachments.map((a) => [a.localId, a.serverId]))
  const context = { ...item.action.context }
  for (const a of item.attachments) {
    const list = context[a.field]
    if (Array.isArray(list)) context[a.field] = list.map((x) => (typeof x === 'string' ? (server.get(x) ?? x) : x))
  }
  return { ...item.action, context }
}
