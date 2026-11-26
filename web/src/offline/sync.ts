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
//   session_account_changed  the browser's session is another account's than the queue's:
//                            sending pauses, and nothing is counted against the item
//   no answer, 5xx, 408, 425, 429
//                            transient: try again later, backing off, up to MAX_ATTEMPTS times,
//                            then it needs attention and the person says when to try again
//   403, 404                 access was lost (the space, the app, the right to add): the item
//                            needs attention, and if the app can no longer be opened at all the
//                            downloaded copy is removed; it says nothing of an earlier send of the
//                            same change, which may be on the server (`unconfirmed` stays)
//   409 (archived, outdated) and 400/422 (the server refused it)
//                            needs attention: not retried until the person says so; the input is
//                            kept
//   409 app_record_conflict  (an update or a delete) the record changed since the change was based
//                            on it: needs attention with the record as it is now, if the person
//                            may read it; nothing is overwritten
//   422 app_row_not_found    (an update or a delete) the record is gone (deleted, or no longer the
//                            person's to change): needs attention; it is never brought back by the
//                            change
//   a field that doesn't fit the app as it is now (removed, retyped, now required, a choice that
//   is no longer one): needs attention as a schema problem; the input is kept for repair

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
// How long a claim on an item holds without being renewed, and how soon a tab that found another
// one sending looks again.
export const LEASE_MS = 30_000
export const LOCK_RETRY_MS = 1_500
// The server's answer to a request made for another account than the session's.
export const ACCOUNT_CHANGED = 'session_account_changed'

// The wait before attempt n+1 after n failures: doubling, capped, with a little jitter so many
// devices coming back together don't arrive together.
export function backoff(attempts: number, random: () => number = Math.random): number {
  const wait = Math.min(MAX_DELAY, BASE_DELAY * 2 ** Math.max(0, attempts - 1))
  return Math.round(wait * (0.85 + random() * 0.3))
}

type Outcome =
  | { kind: 'synced' }
  | { kind: 'paused' }
  // The session is another account's: nothing is sent until it is this queue's again.
  | { kind: 'account' }
  // Not ours to send now (another tab holds it): left alone, looked at again when its claim ends.
  // `ordered`: an earlier change to the same record had no answer for good when the claim was made.
  | { kind: 'skipped'; until?: number; ordered?: boolean }
  // `exhausted`: it failed for the last time and now needs attention: the items behind it are not
  // held back by it.
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

// The server's codes for a value that doesn't fit the field it is for, or a field the app doesn't
// have: when an item that was fine when it was made gets one of these, the app changed under it.
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

// What a failed send means; the item's attachments say whether a photo was the cause. `kind` is
// what the item does; `restoring`, that the step that failed was the restore of a deleted record.
export function classify(e: unknown, kind: ChangeKind = 'insert', restoring = false): Outcome {
  const error = describe(e)
  if (!(e instanceof ApiError)) return { kind: 'transient', error }
  const { status, code } = e
  if (code === ACCOUNT_CHANGED) return { kind: 'account' }
  if (status === 401) return { kind: 'paused' }
  // The storage is used up: a retry fails the same way until someone makes room, so it is for the
  // person to settle (the server's refusal is shown, translated), not tried again on a timer.
  if (storageFull(e)) return { kind: 'attention', reason: 'refused', error }
  if (status >= 500 || status === 408 || status === 425 || status === 429) return { kind: 'transient', error }
  if (code === 'app_attachment_bad' || code === 'app_upload_state') return { kind: 'attention', reason: 'photo', error }
  if (code === 'app_record_conflict' && status === 409) {
    const current = (e.body as { current?: unknown } | undefined)?.current
    const here = current && typeof current === 'object' ? (current as Record<string, unknown>) : null
    // The restore found the record deleted again, at a revision newer than the one the person was
    // shown (there is no live record to show): it is a deleted record still, and is offered a
    // restore again, at the revision it has now.
    if (restoring && !here) return { kind: 'attention', reason: 'deleted', error }
    return { kind: 'attention', reason: 'conflict', error, conflict: { current: here } }
  }
  // Not allowed to restore: the person's role, said as the server's refusal of this change.
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

// Whether a failure is the server's verdict on the operation itself: what it says after it has
// looked the operation up (a change it applied is answered as it was, never refused), so that it
// doesn't have the change. What the server refuses before that (the person's access, an archived
// or unpublished app, a missing release) says nothing of an earlier send, and neither does a
// photo that didn't go up.
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

// An item that has an earlier change to the same record that the server hasn't answered for good
// is not sent: a record's changes are answered in the order they were made, so that what a delete
// finds on the server is whatever the edit before it made of it, never the other way round. An
// answer is for good when the server has the change (synced) or refused it (it needs the person,
// and was not left unconfirmed).
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

// Why an item needs the person to settle it, not another try.
export const RESOLVE = new Set<AttentionReason>(['conflict', 'deleted', 'schema'])

export type SyncStatus = {
  // Why nothing is being sent, if so: the session ended, the browser is signed in as someone
  // else than the queue's owner, or the person said so.
  // 'signing-out': the person's sign-out is certain and not finished; nothing more is sent for them.
  paused: null | 'session' | 'account' | 'manual' | 'signing-out'
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
  // The account this browser says is signed in now (null: nobody knows). A queue is sent only as
  // its owner: when this says someone else, sending pauses before a request is made.
  signedIn?: () => number | null
  // Asks the server whether what the device holds may still be held. Called when the network comes
  // back, before the queue is sent.
  revalidate?: () => Promise<unknown>
  // Runs fn every ms until the returned function is called: what keeps an item's claim alive while
  // its request is in flight. A test's own.
  heartbeat?: (fn: () => void, ms: number) => () => void
}

// An item put back in the queue for another try, at once.
const retried = (i: OutboxItem): OutboxItem => ({
  ...i,
  state: 'queued',
  attempts: 0,
  nextAttemptAt: undefined,
  attention: undefined,
  error: undefined,
  // A photo that was refused is uploaded again from the start.
  attachments: i.attachments.map((a) => (a.state === 'uploaded' && !a.serverId ? { ...a, state: 'pending' } : a)),
})

const realHeartbeat = (fn: () => void, ms: number) => {
  const handle = setInterval(fn, ms)
  return () => clearInterval(handle)
}

// The queue's owner is signing out (in this tab or another, offline/pendingSignOut.ts): nothing more
// is sent for them, and the claim is let go, until the sign-out is resolved one way or the other.
class Departing extends Error {
  constructor() {
    super('the account is signing out')
    this.name = 'Departing'
  }
}

// An item's claim is no longer ours: another tab has it, so nothing of this send is written.
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
  // Something asked to send while a pass was running: another pass follows it.
  private rerun = false
  private paused: SyncStatus['paused'] = null
  private stopped = false
  // Who holds the claims this engine makes on items.
  private readonly owner = randomId()
  private readonly now: () => number
  private readonly upload: typeof uploadPhoto
  private readonly request: typeof api
  private lastRenew = 0
  // The requests of sends in flight: aborted when the claim is lost, or the engine stops.
  private readonly inflight = new Set<AbortController>()
  // Settles when the pass that is running is over.
  private active: Promise<void> | undefined
  // The earliest that a pass that found another tab sending looks again.
  private lockedUntil: number | undefined

  constructor(
    private readonly store: OfflineStore,
    private readonly deps: SyncDeps = {},
  ) {
    this.now = deps.now ?? Date.now
    this.upload = deps.upload ?? uploadPhoto
    this.request = deps.request ?? api
    // Whatever this engine's pages write is written only while the account signed in here is its
    // owner, asked in the transaction that writes.
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

  // Whether the browser's signed-in account is this queue's owner (or isn't known to be anyone
  // else): a queue is only ever sent as its owner.
  private ownerSignedIn(): boolean {
    const who = this.deps.signedIn?.()
    return who === undefined || who === null || who === this.store.user
  }

  // Every request made for the queue says whose it is: the server refuses it for any other session.
  private get headers() {
    return forAccount(this.store.user)
  }

  // For what the person settles by hand (offline/choices.ts): the same, and whether this engine
  // still acts for its owner (it runs, and the account signed in here is theirs).
  ownerHeaders(): Record<string, string> {
    return this.headers
  }

  canAct(): boolean {
    return !this.stopped && this.ownerSignedIn()
  }

  // Starts sending whenever there is something to send: now, when the network comes back, when the
  // page comes to the front, and when an item's wait is over.
  start(): () => void {
    this.stopped = false
    const kick = () => void this.run()
    // The network is back: what the device holds is asked about first (a copy may have been
    // revoked, and an account may have changed meanwhile), then the queue is sent.
    const online = () => void this.revalidated().then(() => this.run())
    window.addEventListener('online', online)
    const visible = () => document.visibilityState === 'visible' && kick()
    document.addEventListener('visibilitychange', visible)
    const lost = (e: Event) => concernsAccount(e, this.store.user) && this.pause('session')
    window.addEventListener(SESSION_LOST, lost)
    // Another tab signed in as someone else: this queue stops being sent from here at once.
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

  // Resolves once the pass that is running (if any) has written what it was doing: what a page
  // letting go of this engine waits for before closing the database.
  idle(): Promise<void> {
    return this.active && this.running ? this.active : Promise.resolve()
  }

  // Asks the server whether what the device holds may still be held, once at a time: a pass that
  // starts while it is in flight (the page coming to the front, a timer, the person's own "try
  // again") waits for it, so nothing is sent on the strength of an answer not yet given.
  private revalidating: Promise<void> | undefined
  private revalidated(): Promise<void> {
    if (this.stopped || this.paused || !this.deps.revalidate) return Promise.resolve()
    this.revalidating ??= (async () => {
      try {
        await this.deps.revalidate?.()
      } catch {
        // The server can't be asked, or the database is closed: the queue's own sends find out.
      }
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
  //
  // A person's press on one item carries the item as it was shown (`seen`): in the transaction that
  // changes it, one that moved on meanwhile (another tab claimed it, sent it, edited it) is refused
  // (`ItemChanged`) and nothing is applied or sent.
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
      // "Sync now" for the whole queue leaves alone what only the person can settle: sending it
      // again would be answered the same way.
      if (id === undefined && item.state === 'attention' && item.attention && RESOLVE.has(item.attention)) continue
      if (item.state === 'queued' || item.state === 'attention') {
        // Decided on the item as it is stored: one a tab just synced is never put back.
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

  // Holds an item as a local draft: it stays on this device and isn't sent until it is sent on purpose.
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

  // Sends what is waiting. One run at a time here and, where the browser has Web Locks, in all of
  // this user's tabs (two tabs sending the same item would still make it once, by the operation
  // key, but would upload its photos twice: the claims on items are what keeps that from
  // happening where there are no locks). A run goes on until nothing is left to send: an item
  // added while it ran is sent by it, and a call made during a run makes another follow.
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
    // When to look again, apart from the items' own waits: a claim that ends, a tab that is sending.
    let wake: number | undefined
    this.lockedUntil = undefined
    try {
      do {
        this.rerun = false
        wake = await this.pass()
      } while (this.rerun && !this.stopped && !this.paused)
    } catch {
      // The database was closed (signing out, or deleted by another tab) while this ran.
    } finally {
      this.running = false
      finished()
      this.emit()
    }
    // Wake up when the first item that is waiting is due.
    try {
      // An item behind an earlier change to its record is not woken by its own wait: it can't be
      // sent before that one is answered, and when it is (here, in this pass, or by the person's
      // choice, which asks for a pass) the pass looks for it again. Its own overdue wait would
      // otherwise set a zero-delay timer that finds nothing, for ever.
      const queue = await this.store.listItems()
      const waiting = queue
        .filter((i) => i.state === 'queued' && i.nextAttemptAt !== undefined && !waitsForEarlier(i, queue))
        .map((i) => i.nextAttemptAt as number)
      let at = Math.min(...waiting, wake ?? Number.POSITIVE_INFINITY)
      // Another tab holds the lock: no sooner than the retry delay, whatever an item's own wait
      // says (one that is already over would make this look again at once, for ever).
      if (this.lockedUntil !== undefined) at = Math.max(at, this.lockedUntil)
      if (Number.isFinite(at)) this.schedule(at)
    } catch {
      // The database was closed (signing out) while this pass ran: nothing is waiting any more.
    }
  }

  private due(item: OutboxItem): boolean {
    const sendable = item.state === 'queued' || item.state === 'syncing'
    return sendable && !(item.nextAttemptAt !== undefined && item.nextAttemptAt > this.now())
  }

  // One tab at a time sends, where Web Locks say so; the answer is when to look again, if
  // something was left that this didn't send.
  private async pass(): Promise<number | undefined> {
    // What the device holds is asked about before anything is sent, whoever asked for the pass.
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
      // Another tab is sending: this one's items are its to send as well, if it gets to them. A tab
      // that skipped doesn't just trust that: it looks again soon, and the lock's holder may have
      // been past the point where it would have seen an item added a moment ago.
      const queue = await this.store.listItems()
      if (queue.some((i) => this.due(i) && !waitsForEarlier(i, queue))) {
        wake = this.now() + LOCK_RETRY_MS
        this.lockedUntil = wake
      }
    })
    return wake
  }

  // Sends the items in the order they were made, until none is left that can be sent now. An item
  // is looked for again after each send, so one made meanwhile is found. `exclusive`: a lock says
  // nobody else is sending, so a claim another page left behind (it was closed) is taken at once.
  private async sendAll(exclusive: boolean): Promise<number | undefined> {
    const done = new Set<string>()
    let wake: number | undefined
    // Items the claim refused to put ahead of an earlier change to their record: the earlier one is
    // sent first (it is looked for again), and this one after it. Bounded, so a queue that keeps
    // changing under the pass can't hold it for ever.
    let reordered = 0
    for (;;) {
      if (this.stopped || this.paused) return wake
      // The queue is the signed-in account's: when someone else is, it waits for its owner.
      if (!this.ownerSignedIn()) {
        this.pause('account')
        return wake
      }
      // Its owner is signing out, here or in another tab: nothing is claimed. Looked at again soon,
      // since a sign-out the server didn't confirm may be taken back.
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
      // The network is down: the items behind this one would find the same. (An item that has run
      // out of tries is the person's to deal with now: it holds nothing back.)
      if (outcome.kind === 'transient' && !outcome.exhausted) return wake
    }
  }

  // The claim on an item, made in its own transaction: taken if nobody holds it, if the holder's
  // time has run out, or if it is ours already (or we hold the lock). Not taken otherwise.
  //
  // `queue` is the item's app's queue as the claiming transaction reads it: a record's changes are
  // answered in the order they were made, and that is decided here, atomically with the claim, not
  // on the list the pass chose from (the person may have put an earlier item of the record back to
  // waiting since). `ordered` is set when that is why nothing was claimed.
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
      // A `syncing` item found here is being sent by a tab, or was by a page that was closed.
      if (!exclusive && i.lease && i.lease.owner !== this.owner && i.lease.until > now) return undefined
    } else {
      return undefined
    }
    // A change to an existing record may be on the server from the moment it is sent, whatever
    // the answer is: until a definite one, nothing else is written to that record's item.
    const changes = kindOf(i) !== 'insert'
    return {
      ...i,
      state: 'syncing',
      sends: (i.sends ?? 0) + 1,
      lease: { owner: this.owner, until: now + LEASE_MS, attempt },
      ...(changes && {
        unconfirmed: true,
        // What an answer that settles nothing goes back to. An item found *syncing* was claimed by
        // a send that never answered for good (its tab was closed, or froze): that send may have
        // reached the server, so a claim made over it is made on an item that is unconfirmed, and
        // an answer that says nothing of the operation (a refusal of the route) leaves it so.
        unconfirmedBefore: i.state === 'syncing' ? true : i.unconfirmed === true,
      }),
    }
  }

  // Changes an item only while the claim is the one this send made (our own, and this attempt's:
  // a claim made again, or by another tab, is another attempt's), and the server hasn't
  // acknowledged it: what a send that lost its claim would write is never written.
  private async owned(
    id: string,
    attempt: string,
    change: (i: OutboxItem) => OutboxItem,
    bump = false,
  ): Promise<OutboxItem> {
    // An engine that was stopped, or whose account is no longer the one signed in here, writes
    // nothing more.
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

  // Keeps the claim while a long upload goes on.
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

  // Whether the queue's owner has a sign-out not resolved on this browser (any tab's).
  private departing(): boolean {
    return signOutPending(this.store.user)
  }

  private notDeparting() {
    if (this.departing()) throw new Departing()
  }

  private async send(start: OutboxItem, exclusive: boolean): Promise<Outcome> {
    const id = start.id
    // The token of this send: what it writes is written only while the item's claim carries it.
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
    // The claim is kept for as long as this send is in flight, on a clock of its own: a request
    // that stalls (a chunk that never answers) makes no progress to renew on, and a lease that ran
    // out would let another tab start the same upload. If the claim is lost anyway (another tab
    // took it, the item was discarded) the request is aborted at once.
    const abort = new AbortController()
    let lost = false
    this.inflight.add(abort)
    // Whether the step that is in flight is the restore of a deleted record.
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
      // Photos first: the record is sent once every one of them is finalized on the server.
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
      // Still ours? If another tab took over, it sends: this one stops before it asks the server.
      await this.renew(id, attempt, true)
      // A change the person chose to apply to a record deleted on the server: the record is
      // restored first, explicitly, and the change is sent on top of the restored revision.
      // Asked again right before anything is sent: a sign-out may have begun during the uploads.
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
        // The photos are the ones persisted on the item: a retry sends the same request. A change
        // to a record says the revision it is based on, which the server checks.
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
      // Stopped, the claim lost, or the account changed while the answer came in: it is not written
      // (the item is sent again by whoever has it next, and the server answers the same operation
      // the same way).
      this.assertLive(abort.signal)
      // The record as the server answered an update, with the revision it has: shown from then on
      // (a replay answers with the record as it is now, which may hold another device's change,
      // and a change that set what was set already makes no revision of its own). Its revision is
      // never paired with the values this device made.
      const ack = kind === 'update' ? recordIn(answer, fresh.record.id) : undefined
      // The server's answer is what makes it synced: this exact operation, sent by this very attempt.
      // An item that was requeued under another key meanwhile, or claimed by another send, is not
      // what this answer is about: its own send gets its own answer (a replay of the same
      // operation is answered the same way).
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
      // A send whose engine stopped writes nothing and lets its claim go; one that lost its claim
      // writes nothing either: another tab has it.
      if (this.stopped && abort.signal.aborted) return await this.letGo(id, attempt)
      if (lost || e instanceof LeaseLost) return { kind: 'skipped' }
      if (e instanceof Departing) {
        await this.letGo(id, attempt)
        return { kind: 'skipped', until: this.now() + LOCK_RETRY_MS }
      }
      if (e instanceof UploadCancelled) return { kind: 'transient', error: describe(e) }
      // Awaited here, inside the try: the request is in flight (and abortable) until it is over.
      return await this.failed(id, attempt, e, abort.signal, () => lost, kindOf(claimed), restoring)
    } finally {
      beat()
      this.inflight.delete(abort)
    }
  }

  // Restores the record the item's change is for (it was deleted on the server, and the person
  // chose to apply the change anyway) and writes the revision it now has. A restore that was made
  // by an earlier send whose answer was lost finds the record live at the very next revision:
  // that is the same restore, and the change goes on from it.
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

  // The claim is let go, so the item is the next sender's at once, not when the lease runs out.
  // (Nothing is counted against it.)
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
    // The first finalized id an item gets is the one it keeps: a send made again (after a failure, by
    // another page) is the same request, and the server holds the first answer for its operation.
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
        // A photo the server has finalized stays so.
        attachments: i.attachments.map((a) => (a.localId === localId && !a.serverId ? { ...a, ...change } : a)),
      }),
      false,
    )
  }

  // Whether what a send found out is still to be acted on: this engine runs, the send's own request
  // wasn't aborted (it stopped, or the claim was lost), and the account signed in here is the
  // queue's owner. Checked after every wait, before anything is written or deleted.
  private live(signal: AbortSignal): boolean {
    return !this.stopped && !signal.aborted && this.ownerSignedIn()
  }

  // The same, as a refusal to go on: what is found out after a wait is acted on only while the
  // account that made the queue is the one signed in (the account's epoch).
  private assertLive(signal: AbortSignal): void {
    if (!this.live(signal)) throw new DOMException('the send was aborted', 'AbortError')
  }

  // Records what a failed send means for the item, and acts on a revocation.
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
    // A 403 or 404 only means the app can't be reached *now*: it is confirmed by asking for the
    // app itself, which is answered for anyone who may open it.
    if (outcome.kind === 'attention' && outcome.reason === 'access') {
      const item = await this.store.getItem(id)
      if (item) {
        const revoked = await this.revoked(item, signal)
        // An answer that came after this send was let go of says nothing that may be acted on.
        if (!this.live(signal)) return lost() ? { kind: 'skipped' } : this.letGo(id, attempt)
        // Nor does one that came after the claim was lost: whoever has the item now (another tab, a
        // send after a retry, a download made meanwhile) asks again for itself. Decided on the item
        // as stored now (its operation and this attempt's token), before any effect of the reply.
        if (revoked && !(await this.stillMine(id, attempt, item.operation))) return { kind: 'skipped' }
        outcome = { ...outcome, revoked }
        if (revoked) await this.deps.onRevoked?.(item, outcome.error)
      }
    }
    if (!this.live(signal)) return lost() ? { kind: 'skipped' } : this.letGo(id, attempt)
    // The wrong account is not the item's failure: it waits, as it was, for its owner.
    const result = outcome
    try {
      await this.owned(id, attempt, (i) => {
        const free = { ...i, lease: undefined }
        switch (result.kind) {
          case 'paused':
          case 'account':
            // Waits for the next session: not a failed attempt. That the session ended says nothing
            // of an earlier send that went unanswered, which may be on the server: only a definite
            // answer clears that.
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
              // Only a definite answer about this operation says whether the server has the change:
              // a verdict it gave after it checked the operation's receipt. A refusal made before
              // it looked (access lost, an archived space, a photo) says nothing of an earlier send,
              // so the item is as unconfirmed as it was before this one.
              unconfirmed: definite(result) ? undefined : i.unconfirmedBefore === true ? true : undefined,
              unconfirmedBefore: undefined,
              // A restore that found the record deleted again is asked for again, at its new revision.
              restore: result.reason === 'deleted' ? undefined : i.restore,
              conflict: result.conflict,
              problems: result.problems,
              // A photo the server no longer has is uploaded again when the person retries.
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

  // Whether the item is still claimed by this very send (its operation key and attempt token), read
  // in a transaction of its own that writes nothing.
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

// Record `id` as the messages an action answers with say it (an item: its id, `_revision` and
// fields), if they do: the records are at /items, or in the whole data model.
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
      // Only the record's own keys: what the device adds to what it shows is not the server's.
      return Object.fromEntries(
        Object.entries(found).filter(([k]) => k === 'id' || k === '_revision' || !k.startsWith('_')),
      )
    }
  }
  return undefined
}

// The revision of record `id` in the messages an action answers with, if they say it.
export const revisionIn = (answer: unknown, id: number): number | undefined =>
  recordIn(answer, id)?._revision as number | undefined

// The action to send: the context's local photo ids replaced by the server's.
export function withServerPhotos(item: OutboxItem): OutboxItem['action'] {
  const server = new Map(item.attachments.map((a) => [a.localId, a.serverId]))
  const context = { ...item.action.context }
  for (const a of item.attachments) {
    const list = context[a.field]
    // A photo field holds a list of ids; a signature field holds one, as text or as a list of one.
    if (typeof list === 'string') context[a.field] = server.get(list) ?? list
    else if (Array.isArray(list)) context[a.field] = list.map((x) => (typeof x === 'string' ? (server.get(x) ?? x) : x))
  }
  return { ...item.action, context }
}
