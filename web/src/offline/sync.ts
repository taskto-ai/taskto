import { ApiError, api, concernsAccount, forAccount, SESSION_LOST } from '../api'
import { storageFull, UploadCancelled, uploadPhoto } from '../apps/photos'
import { signOutPending } from './pendingSignOut'
import {
  type Attachment,
  type AttentionReason,
  type ChangeKind,
  type Conflict,
  type ItemError,
  kindOf,
  type OfflineStore,
  type OutboxItem,
  type Problem,
  type SeenAs,
} from './store'

export const MAX_ATTEMPTS = 8
const MAX_REORDERS = 50
const BASE_DELAY = 2_000
const MAX_DELAY = 5 * 60_000
export const LEASE_MS = 30_000
export const LOCK_RETRY_MS = 1_500
export const ACCOUNT_CHANGED = 'session_account_changed'

export function backoff(attempts: number, random: () => number = Math.random): number {
  const wait = Math.min(MAX_DELAY, BASE_DELAY * 2 ** Math.max(0, attempts - 1))
  return Math.round(wait * (0.85 + random() * 0.3))
}

type Outcome =
  | { kind: 'synced' }
  | { kind: 'paused' }
  | { kind: 'account' }
  | { kind: 'skipped'; until?: number; ordered?: boolean }
  | { kind: 'transient'; error: ItemError; exhausted?: boolean }
  | {
      kind: 'attention'
      reason: AttentionReason
      error: ItemError
      revoked?: boolean
      conflict?: Conflict
      problems?: Problem[]
    }

export function describe(e: unknown): ItemError {
  if (e instanceof ApiError) return { status: e.status, code: e.code, params: e.params, message: e.message }
  return { message: e instanceof Error ? e.message : String(e) }
}

const SCHEMA_CODES = new Set([
  'app_field_required',
  'app_field_required_when',
  'app_field_not_option',
  'app_field_not_number',
  'app_field_not_date',
  'app_field_not_time',
  'app_field_files',
  'bad_request',
])

export function classify(e: unknown, kind: ChangeKind = 'insert', restoring = false): Outcome {
  const error = describe(e)
  if (!(e instanceof ApiError)) return { kind: 'transient', error }
  const { status, code } = e
  if (code === ACCOUNT_CHANGED) return { kind: 'account' }
  if (status === 401) return { kind: 'paused' }
  if (storageFull(e)) return { kind: 'attention', reason: 'refused', error }
  if (status >= 500 || status === 408 || status === 425 || status === 429) return { kind: 'transient', error }
  if (code === 'app_attachment_bad' || code === 'app_upload_state') return { kind: 'attention', reason: 'photo', error }
  if (code === 'app_record_conflict' && status === 409) {
    const current = (e.body as { current?: unknown } | undefined)?.current
    const here = current && typeof current === 'object' ? (current as Record<string, unknown>) : null
    if (restoring && !here) return { kind: 'attention', reason: 'deleted', error }
    return { kind: 'attention', reason: 'conflict', error, conflict: { current: here } }
  }
  if (restoring && (status === 403 || status === 404)) return { kind: 'attention', reason: 'refused', error }
  if (code === 'app_row_not_found' && kind !== 'insert') return { kind: 'attention', reason: 'deleted', error }
  if ((status === 400 || status === 422) && code && SCHEMA_CODES.has(code) && kind !== 'delete') {
    const field = (e.params as { field?: unknown } | undefined)?.field
    return {
      kind: 'attention',
      reason: 'schema',
      error,
      problems: [{ field: typeof field === 'string' ? field : '', code, params: e.params }],
    }
  }
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

export function definite(o: Extract<Outcome, { kind: 'attention' }>): boolean {
  switch (o.reason) {
    case 'conflict':
    case 'deleted':
    case 'schema':
      return true
    case 'refused':
      return o.error.status === 400 || o.error.status === 422 || o.error.code === 'app_operation_reused'
    case 'outdated':
      return o.error.code === 'app_release_outdated'
    default:
      return false
  }
}

export function waitsForEarlier(item: OutboxItem, queue: readonly OutboxItem[]): boolean {
  if (kindOf(item) === 'insert') return false
  return queue.some(
    (e) =>
      e.seq < item.seq &&
      e.scopeKey === item.scopeKey &&
      kindOf(e) !== 'insert' &&
      e.record.id === item.record.id &&
      !(e.state === 'synced' || (e.state === 'attention' && !e.unconfirmed)),
  )
}

export const RESOLVE = new Set<AttentionReason>(['conflict', 'deleted', 'schema'])

export type SyncStatus = {
  paused: null | 'session' | 'account' | 'manual' | 'signing-out'
  running: boolean
  online: boolean
}

export type SyncDeps = {
  now?: () => number
  setTimer?: (fn: () => void, ms: number) => unknown
  clearTimer?: (handle: unknown) => void
  upload?: typeof uploadPhoto
  request?: typeof api
  random?: () => number
  onRevoked?: (item: OutboxItem, why: ItemError) => void | Promise<void>
  signedIn?: () => number | null
  revalidate?: () => Promise<unknown>
  heartbeat?: (fn: () => void, ms: number) => () => void
}

const retried = (i: OutboxItem): OutboxItem => ({
  ...i,
  state: 'queued',
  attempts: 0,
  nextAttemptAt: undefined,
  attention: undefined,
  error: undefined,
  attachments: i.attachments.map((a) => (a.state === 'uploaded' && !a.serverId ? { ...a, state: 'pending' } : a)),
})

const realHeartbeat = (fn: () => void, ms: number) => {
  const handle = setInterval(fn, ms)
  return () => clearInterval(handle)
}

class Departing extends Error {
  constructor() {
    super('the account is signing out')
    this.name = 'Departing'
  }
}

class LeaseLost extends Error {}

const randomId = () => {
  const bytes = new Uint8Array(8)
  globalThis.crypto.getRandomValues(bytes)
  return [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('')
}

export class SyncEngine {
  private listeners = new Set<() => void>()
  private timer: unknown
  private running = false
  private rerun = false
  private paused: SyncStatus['paused'] = null
  private stopped = false
  private readonly owner = randomId()
  private readonly now: () => number
  private readonly upload: typeof uploadPhoto
  private readonly request: typeof api
  private lastRenew = 0
  private readonly inflight = new Set<AbortController>()
  private active: Promise<void> | undefined
  private lockedUntil: number | undefined

  constructor(
    private readonly store: OfflineStore,
    private readonly deps: SyncDeps = {},
  ) {
    this.now = deps.now ?? Date.now
    this.upload = deps.upload ?? uploadPhoto
    this.request = deps.request ?? api
    store.requireAccount(() => this.ownerSignedIn())
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

  private ownerSignedIn(): boolean {
    const who = this.deps.signedIn?.()
    return who === undefined || who === null || who === this.store.user
  }

  private get headers() {
    return forAccount(this.store.user)
  }

  ownerHeaders(): Record<string, string> {
    return this.headers
  }

  canAct(): boolean {
    return !this.stopped && this.ownerSignedIn()
  }

  start(): () => void {
    this.stopped = false
    const kick = () => void this.run()
    const online = () => void this.revalidated().then(() => this.run())
    window.addEventListener('online', online)
    const visible = () => document.visibilityState === 'visible' && kick()
    document.addEventListener('visibilitychange', visible)
    const lost = (e: Event) => concernsAccount(e, this.store.user) && this.pause('session')
    window.addEventListener(SESSION_LOST, lost)
    const storage = () => {
      if (!this.ownerSignedIn() && !this.paused) this.pause('account')
    }
    window.addEventListener('storage', storage)
    kick()
    return () => {
      this.stopped = true
      for (const c of this.inflight) c.abort()
      window.removeEventListener('online', online)
      document.removeEventListener('visibilitychange', visible)
      window.removeEventListener(SESSION_LOST, lost)
      window.removeEventListener('storage', storage)
      this.clear()
    }
  }

  idle(): Promise<void> {
    return this.active && this.running ? this.active : Promise.resolve()
  }

  private revalidating: Promise<void> | undefined
  private revalidated(): Promise<void> {
    if (this.stopped || this.paused || !this.deps.revalidate) return Promise.resolve()
    this.revalidating ??= (async () => {
      try {
        await this.deps.revalidate?.()
      } catch {}
    })().finally(() => {
      this.revalidating = undefined
    })
    return this.revalidating
  }

  pause(reason: 'session' | 'account' | 'manual' | 'signing-out') {
    this.paused = reason
    this.clear()
    this.emit()
  }

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

  async retry(id?: string, seen?: SeenAs): Promise<void> {
    if (id !== undefined && seen) {
      await this.store.updateItem(
        id,
        (i) => (i.state === 'queued' || i.state === 'attention' ? retried(i) : undefined),
        {
          guarded: true,
          seen,
        },
      )
      if (this.paused === 'manual') this.paused = null
      await this.run()
      return
    }
    for (const item of await this.store.listItems()) {
      if (id !== undefined && item.id !== id) continue
      if (id === undefined && item.state === 'attention' && item.attention && RESOLVE.has(item.attention)) continue
      if (item.state === 'queued' || item.state === 'attention') {
        await this.store.updateItem(
          item.id,
          (i) => (i.state !== 'queued' && i.state !== 'attention' ? undefined : retried(i)),
          {
            guarded: true,
          },
        )
      }
    }
    if (this.paused === 'manual') this.paused = null
    await this.run()
  }

  async hold(id: string, seen?: SeenAs): Promise<void> {
    await this.store.updateItem(
      id,
      (i) => (i.state === 'synced' || i.state === 'syncing' ? undefined : { ...i, state: 'draft' }),
      { guarded: true, seen },
    )
    this.emit()
  }

  async release(id: string, seen?: SeenAs): Promise<void> {
    await this.store.updateItem(
      id,
      (i) => (i.state === 'draft' ? { ...i, state: 'queued', attempts: 0, nextAttemptAt: undefined } : undefined),
      { guarded: true, seen },
    )
    await this.run()
  }

  async run(): Promise<void> {
    if (this.stopped || this.paused) return
    if (this.running) {
      this.rerun = true
      return
    }
    if (typeof navigator !== 'undefined' && navigator.onLine === false) return
    this.running = true
    this.emit()
    let finished: () => void = () => {}
    this.active = new Promise<void>((resolve) => {
      finished = resolve
    })
    let wake: number | undefined
    this.lockedUntil = undefined
    try {
      do {
        this.rerun = false
        wake = await this.pass()
      } while (this.rerun && !this.stopped && !this.paused)
    } catch {
    } finally {
      this.running = false
      finished()
      this.emit()
    }
    try {
      const queue = await this.store.listItems()
      const waiting = queue
        .filter((i) => i.state === 'queued' && i.nextAttemptAt !== undefined && !waitsForEarlier(i, queue))
        .map((i) => i.nextAttemptAt as number)
      let at = Math.min(...waiting, wake ?? Number.POSITIVE_INFINITY)
      if (this.lockedUntil !== undefined) at = Math.max(at, this.lockedUntil)
      if (Number.isFinite(at)) this.schedule(at)
    } catch {}
  }

  private due(item: OutboxItem): boolean {
    const sendable = item.state === 'queued' || item.state === 'syncing'
    return sendable && !(item.nextAttemptAt !== undefined && item.nextAttemptAt > this.now())
  }

  private async pass(): Promise<number | undefined> {
    if (this.revalidating) await this.revalidating
    if (this.stopped || this.paused) return undefined
    const locks = typeof navigator !== 'undefined' ? navigator.locks : undefined
    if (!locks) return this.sendAll(false)
    let wake: number | undefined
    await locks.request(`taskto-outbox-${this.store.user}`, { ifAvailable: true }, async (lock) => {
      if (lock) {
        wake = await this.sendAll(true)
        return
      }
      const queue = await this.store.listItems()
      if (queue.some((i) => this.due(i) && !waitsForEarlier(i, queue))) {
        wake = this.now() + LOCK_RETRY_MS
        this.lockedUntil = wake
      }
    })
    return wake
  }

  private async sendAll(exclusive: boolean): Promise<number | undefined> {
    const done = new Set<string>()
    let wake: number | undefined
    let reordered = 0
    for (;;) {
      if (this.stopped || this.paused) return wake
      if (!this.ownerSignedIn()) {
        this.pause('account')
        return wake
      }
      if (this.departing()) return Math.min(wake ?? Number.POSITIVE_INFINITY, this.now() + LOCK_RETRY_MS)
      const queue = await this.store.listItems()
      const next = queue.find((i) => !done.has(i.id) && this.due(i) && !waitsForEarlier(i, queue))
      if (!next) return wake
      done.add(next.id)
      const outcome = await this.send(next, exclusive)
      if (outcome.kind === 'paused') {
        this.pause('session')
        return wake
      }
      if (outcome.kind === 'account') {
        this.pause('account')
        return wake
      }
      if (outcome.kind === 'skipped' && outcome.until !== undefined) {
        wake = Math.min(wake ?? Number.POSITIVE_INFINITY, outcome.until)
      }
      if (outcome.kind === 'skipped' && outcome.ordered && reordered++ < MAX_REORDERS) done.delete(next.id)
      if (outcome.kind === 'transient' && !outcome.exhausted) return wake
    }
  }

  private claim(
    i: OutboxItem,
    exclusive: boolean,
    attempt: string,
    queue: readonly OutboxItem[],
    ordered: { value: boolean },
  ): OutboxItem | undefined {
    const now = this.now()
    if (waitsForEarlier(i, queue)) {
      ordered.value = true
      return undefined
    }
    if (i.state === 'queued') {
      if (i.nextAttemptAt !== undefined && i.nextAttemptAt > now) return undefined
    } else if (i.state === 'syncing') {
      if (!exclusive && i.lease && i.lease.owner !== this.owner && i.lease.until > now) return undefined
    } else {
      return undefined
    }
    const changes = kindOf(i) !== 'insert'
    return {
      ...i,
      state: 'syncing',
      sends: (i.sends ?? 0) + 1,
      lease: { owner: this.owner, until: now + LEASE_MS, attempt },
      ...(changes && {
        unconfirmed: true,
        unconfirmedBefore: i.state === 'syncing' ? true : i.unconfirmed === true,
      }),
    }
  }

  private async owned(
    id: string,
    attempt: string,
    change: (i: OutboxItem) => OutboxItem,
    bump = false,
  ): Promise<OutboxItem> {
    if (this.stopped || !this.ownerSignedIn()) throw new LeaseLost()
    let held = true
    const saved = await this.store.updateItem(
      id,
      (i) => {
        if (i.state === 'synced' || i.lease?.owner !== this.owner || i.lease.attempt !== attempt) {
          held = false
          return undefined
        }
        return change(i)
      },
      { bump },
    )
    if (!held || !saved) throw new LeaseLost()
    return saved
  }

  private async renew(id: string, attempt: string, force = false): Promise<void> {
    const now = this.now()
    if (!force && now - this.lastRenew < LEASE_MS / 3) return
    this.lastRenew = now
    await this.owned(
      id,
      attempt,
      (i) => ({ ...i, lease: { owner: this.owner, until: now + LEASE_MS, attempt } }),
      false,
    )
  }

  private departing(): boolean {
    return signOutPending(this.store.user)
  }

  private notDeparting() {
    if (this.departing()) throw new Departing()
  }

  private async send(start: OutboxItem, exclusive: boolean): Promise<Outcome> {
    const id = start.id
    const attempt = randomId()
    const ordered = { value: false }
    const claimed = await this.store.claimItem(id, (i, queue) => this.claim(i, exclusive, attempt, queue, ordered))
    if (claimed?.state !== 'syncing' || claimed.lease?.owner !== this.owner || claimed.lease.attempt !== attempt) {
      return {
        kind: 'skipped',
        until: claimed?.state === 'syncing' ? claimed.lease?.until : undefined,
        ordered: ordered.value,
      }
    }
    this.lastRenew = this.now()
    const abort = new AbortController()
    let lost = false
    this.inflight.add(abort)
    let restoring = false
    const beat = (this.deps.heartbeat ?? realHeartbeat)(() => {
      this.renew(id, attempt, true).catch((e) => {
        if (e instanceof LeaseLost || this.stopped) {
          lost = true
          abort.abort()
        }
      })
    }, LEASE_MS / 3)
    try {
      for (const a of claimed.attachments) {
        const current = (await this.store.getItem(id))?.attachments.find((x) => x.localId === a.localId)
        if (!current || (current.state === 'uploaded' && current.serverId)) continue
        this.notDeparting()
        await this.uploadOne(claimed, current, abort.signal, attempt)
      }
      let fresh = await this.store.getItem(id)
      if (!fresh) return { kind: 'synced' }
      if (fresh.attachments.some((a) => !(a.state === 'uploaded' && a.serverId))) {
        throw new ApiError(0, 'a photo is not on the server', { code: 'app_attachment_bad' })
      }
      await this.renew(id, attempt, true)
      this.notDeparting()
      if (fresh.restore) {
        restoring = true
        fresh = await this.restoreFirst(fresh, abort.signal, attempt)
        restoring = false
      }
      const kind = kindOf(fresh)
      this.notDeparting()
      const answer = await this.request<unknown>(`${fresh.base}/actions`, {
        method: 'POST',
        body:
          kind === 'insert'
            ? { ...withServerPhotos(fresh), release: fresh.release, operation: fresh.operation }
            : {
                ...fresh.action,
                release: fresh.release,
                operation: fresh.operation,
                expectedRevision: fresh.record.revision,
              },
        headers: this.headers,
        signal: abort.signal,
      })
      this.assertLive(abort.signal)
      const ack = kind === 'update' ? recordIn(answer, fresh.record.id) : undefined
      const sent = fresh.operation
      let acknowledged = false
      await this.store.updateItem(
        id,
        (i) => {
          if (
            i.state !== 'syncing' ||
            i.operation !== sent ||
            i.lease?.owner !== this.owner ||
            i.lease.attempt !== attempt
          ) {
            return undefined
          }
          acknowledged = true
          return {
            ...i,
            state: 'synced',
            syncedAt: new Date(this.now()).toISOString(),
            attempts: i.attempts + 1,
            nextAttemptAt: undefined,
            attention: undefined,
            error: undefined,
            conflict: undefined,
            problems: undefined,
            restore: undefined,
            unconfirmed: undefined,
            unconfirmedBefore: undefined,
            ack,
            lease: undefined,
          }
        },
        { bump: false },
      )
      if (!acknowledged) return { kind: 'skipped' }
      if (this.ownerSignedIn()) await this.store.dropBlobs(id)
      return { kind: 'synced' }
    } catch (e) {
      if (this.stopped && abort.signal.aborted) return await this.letGo(id, attempt)
      if (lost || e instanceof LeaseLost) return { kind: 'skipped' }
      if (e instanceof Departing) {
        await this.letGo(id, attempt)
        return { kind: 'skipped', until: this.now() + LOCK_RETRY_MS }
      }
      if (e instanceof UploadCancelled) return { kind: 'transient', error: describe(e) }
      return await this.failed(id, attempt, e, abort.signal, () => lost, kindOf(claimed), restoring)
    } finally {
      beat()
      this.inflight.delete(abort)
    }
  }

  private async restoreFirst(item: OutboxItem, signal: AbortSignal, attempt: string): Promise<OutboxItem> {
    const at = item.restore?.revision ?? 0
    let revision: number
    try {
      const record = await this.request<{ revision: number }>(`${item.base}/records/${item.record.id}/restore`, {
        method: 'POST',
        body: { expected_revision: at },
        headers: this.headers,
        signal,
      })
      revision = record.revision
    } catch (e) {
      const current =
        e instanceof ApiError ? (e.body as { current?: { _revision?: unknown } } | undefined)?.current : undefined
      if (!(e instanceof ApiError && e.code === 'app_record_conflict' && current?._revision === at + 1)) throw e
      revision = at + 1
    }
    this.assertLive(signal)
    return this.owned(item.id, attempt, (i) => ({ ...i, restore: undefined, record: { ...i.record, revision } }))
  }

  private async letGo(id: string, attempt: string): Promise<Outcome> {
    await this.store
      .updateItem(
        id,
        (i) =>
          i.state === 'syncing' && i.lease?.owner === this.owner && i.lease.attempt === attempt
            ? { ...i, state: 'queued', lease: undefined }
            : undefined,
        { bump: false },
      )
      .catch(() => {})
    return { kind: 'skipped' }
  }

  private async uploadOne(item: OutboxItem, a: Attachment, signal: AbortSignal, attempt: string): Promise<void> {
    const blob = await this.store.getBlob(a.blobId)
    if (!blob) throw new ApiError(0, 'the photo is no longer on this device', { code: 'app_attachment_bad' })
    await this.patchAttachment(item.id, attempt, a.localId, { state: 'uploading' })
    const done = await this.upload(item.base, a.field, blob, {
      resume: a.uploadId,
      onStarted: (uploadId) => void this.patchAttachment(item.id, attempt, a.localId, { uploadId }).catch(() => {}),
      onProgress: () => void this.renew(item.id, attempt).catch(() => {}),
      headers: this.headers,
      signal,
      retries: 2,
    })
    this.assertLive(signal)
    await this.owned(
      item.id,
      attempt,
      (i) => ({
        ...i,
        attachments: i.attachments.map((x) =>
          x.localId === a.localId
            ? x.serverId
              ? x
              : { ...x, state: 'uploaded', serverId: done.id, uploadId: done.id }
            : x,
        ),
      }),
      false,
    )
  }

  private patchAttachment(id: string, attempt: string, localId: string, change: Partial<Attachment>) {
    return this.owned(
      id,
      attempt,
      (i) => ({
        ...i,
        attachments: i.attachments.map((a) => (a.localId === localId && !a.serverId ? { ...a, ...change } : a)),
      }),
      false,
    )
  }

  private live(signal: AbortSignal): boolean {
    return !this.stopped && !signal.aborted && this.ownerSignedIn()
  }

  private assertLive(signal: AbortSignal): void {
    if (!this.live(signal)) throw new DOMException('the send was aborted', 'AbortError')
  }

  private async failed(
    id: string,
    attempt: string,
    e: unknown,
    signal: AbortSignal,
    lost: () => boolean,
    kind: ChangeKind,
    restoring: boolean,
  ): Promise<Outcome> {
    let outcome = classify(e, kind, restoring)
    if (outcome.kind === 'attention' && outcome.reason === 'access') {
      const item = await this.store.getItem(id)
      if (item) {
        const revoked = await this.revoked(item, signal)
        if (!this.live(signal)) return lost() ? { kind: 'skipped' } : this.letGo(id, attempt)
        if (revoked && !(await this.stillMine(id, attempt, item.operation))) return { kind: 'skipped' }
        outcome = { ...outcome, revoked }
        if (revoked) await this.deps.onRevoked?.(item, outcome.error)
      }
    }
    if (!this.live(signal)) return lost() ? { kind: 'skipped' } : this.letGo(id, attempt)
    const result = outcome
    try {
      await this.owned(id, attempt, (i) => {
        const free = { ...i, lease: undefined }
        switch (result.kind) {
          case 'paused':
          case 'account':
            return {
              ...free,
              state: 'queued',
              unconfirmed: i.unconfirmedBefore === true ? true : undefined,
              unconfirmedBefore: undefined,
            }
          case 'transient': {
            const attempts = i.attempts + 1
            if (attempts >= MAX_ATTEMPTS) {
              result.exhausted = true
              return {
                ...free,
                state: 'attention',
                attention: 'gave_up',
                attempts,
                nextAttemptAt: undefined,
                error: result.error,
              }
            }
            return {
              ...free,
              state: 'queued',
              attempts,
              nextAttemptAt: this.now() + backoff(attempts, this.deps.random),
              error: result.error,
            }
          }
          case 'attention':
            return {
              ...free,
              state: 'attention',
              attention: result.reason,
              nextAttemptAt: undefined,
              error: result.error,
              unconfirmed: definite(result) ? undefined : i.unconfirmedBefore === true ? true : undefined,
              unconfirmedBefore: undefined,
              restore: result.reason === 'deleted' ? undefined : i.restore,
              conflict: result.conflict,
              problems: result.problems,
              attachments:
                result.reason === 'photo'
                  ? i.attachments.map((a) => ({ ...a, state: 'pending', uploadId: undefined, serverId: undefined }))
                  : i.attachments,
            }
          default:
            return i
        }
      })
    } catch (err) {
      if (!(err instanceof LeaseLost)) throw err
    }
    return outcome
  }

  private async stillMine(id: string, attempt: string, operation: string): Promise<boolean> {
    let mine = false
    try {
      await this.store.updateItem(
        id,
        (i) => {
          mine =
            i.state === 'syncing' &&
            i.operation === operation &&
            i.lease?.owner === this.owner &&
            i.lease.attempt === attempt
          return undefined
        },
        { bump: false },
      )
    } catch {
      return false
    }
    return mine && !this.stopped && this.ownerSignedIn()
  }

  private async revoked(item: OutboxItem, signal: AbortSignal): Promise<boolean> {
    try {
      await this.request(item.base, { headers: this.headers, signal })
      return false
    } catch (e) {
      return e instanceof ApiError && (e.status === 403 || e.status === 404)
    }
  }
}

export function recordIn(answer: unknown, id: number): Record<string, unknown> | undefined {
  if (!Array.isArray(answer)) return undefined
  for (const m of answer as { updateDataModel?: { path?: string; value?: unknown } }[]) {
    const update = m?.updateDataModel
    if (!update) continue
    const list =
      update.path === '/'
        ? (update.value as { items?: unknown } | undefined)?.items
        : update.path === '/items'
          ? update.value
          : undefined
    if (!Array.isArray(list)) continue
    const found = list.find((i) => (i as { id?: unknown } | null)?.id === id) as
      | (Record<string, unknown> & { _revision?: unknown })
      | undefined
    if (found && typeof found._revision === 'number') {
      return Object.fromEntries(
        Object.entries(found).filter(([k]) => k === 'id' || k === '_revision' || !k.startsWith('_')),
      )
    }
  }
  return undefined
}

export const revisionIn = (answer: unknown, id: number): number | undefined =>
  recordIn(answer, id)?._revision as number | undefined

export function withServerPhotos(item: OutboxItem): OutboxItem['action'] {
  const server = new Map(item.attachments.map((a) => [a.localId, a.serverId]))
  const context = { ...item.action.context }
  for (const a of item.attachments) {
    const list = context[a.field]
    if (typeof list === 'string') context[a.field] = server.get(list) ?? list
    else if (Array.isArray(list)) context[a.field] = list.map((x) => (typeof x === 'string' ? (server.get(x) ?? x) : x))
  }
  return { ...item.action, context }
}
