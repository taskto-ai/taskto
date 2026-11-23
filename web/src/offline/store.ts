// What the device keeps for offline use, one IndexedDB database per signed-in user:
// `taskto-offline-<user id>`. Nothing of one account is ever in another's database, so an
// account signing in on the same browser can't read, write or even open the last one's, and
// signing out (or discarding) deletes the database itself (docs/offline.md).
//
// Four stores:
//   downloads  an app's downloaded release and records, one per (workspace, space, app)
//   outbox     the records made offline and waiting for the server, with where they go
//   blobs      the photos of queued records, which stay until the server has them
//   meta       small facts (the sequence for new items)
//
// The schema is versioned. A version's migration runs once, in the upgrade's transaction, and a
// database written by a newer version than this code reads is not touched (SchemaTooNew): the
// page then says the app must be updated, which is what keeps a service worker update from ever
// losing a queue it can't read (the update waits for the person: docs/pwa.md).

import type { OfflineApp } from '../api'
import { deleteDatabase, type Migration, openDatabase, runTx, type Tx } from './idb'

export const SCHEMA_VERSION = 2

export const DOWNLOADS = 'downloads'
export const OUTBOX = 'outbox'
export const BLOBS = 'blobs'
export const META = 'meta'
// The meta row that says the database is being deleted: nothing is written to it from then on.
const RETIRED = 'retired'

// What each version added. A new version appends here (never edits one that shipped) and raises
// SCHEMA_VERSION; an upgrade runs every step the database hasn't had.
export const MIGRATIONS: Record<number, Migration> = {
  1: (db) => {
    db.createObjectStore(DOWNLOADS, { keyPath: 'key' })
    const outbox = db.createObjectStore(OUTBOX, { keyPath: 'id' })
    outbox.createIndex('byApp', 'scopeKey')
    outbox.createIndex('byState', 'state')
    db.createObjectStore(BLOBS, { keyPath: 'id' })
    db.createObjectStore(META, { keyPath: 'key' })
  },
  // Edits and deletions of downloaded records: every item says what kind of change it is. What
  // version 1 queued was only ever inserts, so they are marked so, in this upgrade's transaction.
  2: (_db, tx) => {
    const request = tx.objectStore(OUTBOX).openCursor()
    request.onsuccess = () => {
      const cursor = request.result
      if (!cursor) return
      const item = cursor.value as Partial<OutboxItem>
      if (item.kind === undefined) cursor.update({ ...item, kind: 'insert' })
      cursor.continue()
    }
  },
}

export type Scope = { workspace: number; space: number; app: number }

export const scopeKey = (s: Scope) => `${s.workspace}/${s.space}/${s.app}`

// A downloaded app: the server's download with where it is from and when it was taken.
export type Download = Scope & {
  key: string
  user: number
  name: string
  release: number
  definition: OfflineApp['definition']
  messages: unknown[]
  scopeOf: 'all' | 'own'
  records: number
  total: number
  truncated: boolean
  canInsert: boolean
  capturedAt: string
  // Which of the downloaded records this person's own operations made: operation key → record id.
  // The record an item of the outbox made is among the downloaded ones when its key is here.
  operations: Record<string, number>
  // Which of the downloaded records the person may change or delete (the server's word when the
  // copy was taken). A copy without it (made by an earlier version) offers no offline edits until
  // it is downloaded again.
  modifiable?: number[]
  // About how big the copy is, for the storage the person is shown.
  bytes: number
}

// What an item does: make a record, change a downloaded one, or delete one.
export type ChangeKind = 'insert' | 'update' | 'delete'

export type OutboxState = 'draft' | 'queued' | 'syncing' | 'synced' | 'attention'

// A photo a queued record depends on: kept here (its blob, in BLOBS) until the server has it. The
// record is sent only when every one is `uploaded`, which means the server finalized the upload.
export type Attachment = {
  // The id the record's screen holds for the photo until it has the server's.
  localId: string
  field: string
  blobId: string
  size: number
  type: string
  state: 'pending' | 'uploading' | 'uploaded'
  // The server's upload, to resume after the page was closed, and the attachment it became.
  uploadId?: string
  serverId?: string
}

export type ItemError = { status?: number; code?: string; params?: Record<string, unknown>; message: string }

// Why an item needs attention: the server refused it for good, the person must act, or it kept
// failing. `conflict`: the record changed on the server since the change was based on it;
// `deleted`: the record is no longer there to change; `schema`: what was entered doesn't fit the
// app as it is now.
export type AttentionReason =
  | 'refused'
  | 'access'
  | 'archived'
  | 'outdated'
  | 'gave_up'
  | 'photo'
  | 'conflict'
  | 'deleted'
  | 'schema'

// What doesn't fit, as the person is shown it: a field, and the server's code (or the one the
// check on the device found) with its params, which the web app translates (errors.<code>).
export type Problem = { field: string; code: string; params?: Record<string, unknown> }

// What the server answered to a change made against a record that has moved on: the record as it
// is now (an item: its id, `_revision` and fields), or null when the person may not read it.
export type Conflict = { current: Record<string, unknown> | null }

export type OutboxItem = {
  id: string
  // Order of making; items are sent in it.
  seq: number
  // Who made it, and where it goes.
  user: number
  scopeKey: string
  workspace: number
  space: number
  app: number
  // The routes it is sent to: …/spaces/{space}/apps/{app}.
  base: string
  // The release its screen was drawn from, and that release's definition version.
  release: number
  schema: number
  // The action as the screen sent it, with the photos' local ids in its context.
  action: {
    name: string
    surfaceId: string
    sourceComponentId: string
    timestamp: string
    context: Record<string, unknown>
  }
  // The idempotency key, made once with the item: every send of it is the same request.
  operation: string
  // What it does. Items written by version 1 of the database were all inserts (migrated).
  kind: ChangeKind
  // The record it is about. An insert's: a negative local id until the server has it, and no
  // revision. An update's or a delete's: the downloaded record's id and the revision the change
  // was based on, which the server checks (`expectedRevision`: mandatory for an offline change).
  record: { id: number; revision: number | null }
  // An update's or a delete's record as it was downloaded (its fields, without id and revision):
  // what the person saw when they changed it, to show beside the current one in a conflict.
  original?: Record<string, unknown>
  // For an update or a delete the server acknowledged: the revision it gave the record, so a
  // later change to it is based on that one.
  ackRevision?: number
  // Set once a send was started and the server's answer wasn't a definite one (no answer, 5xx,
  // the page was closed): the change may be on the server. Such an item takes no further edit to
  // its record, and its key is never reused for another request.
  unconfirmed?: boolean
  // The record as the server has it now, when it refused the change as a conflict.
  conflict?: Conflict
  // What doesn't fit the app as it is now, when it needs attention for that.
  problems?: Problem[]
  // A change to a record deleted on the server that the person chose to apply anyway: the
  // record is restored first (at this revision of its deletion), then the change is sent on top.
  restore?: { revision: number }
  attachments: Attachment[]
  state: OutboxState
  attempts: number
  nextAttemptAt?: number
  attention?: AttentionReason
  error?: ItemError
  createdAt: string
  updatedAt: string
  // When the server acknowledged it.
  syncedAt?: string
  // Who is sending it, until when: taken in the item's own transaction before a send, renewed
  // while it goes on, so two tabs (or a tab and a page that was closed) never send one item at
  // once, with or without Web Locks. A lease that has run out is anyone's to take.
  lease?: { owner: string; until: number }
}

export type BlobRow = { id: string; item: string; blob: Blob }

// What an item does: an item that says nothing was made by version 1 of the database, an insert.
export const kindOf = (item: Pick<OutboxItem, 'kind'>): ChangeKind => item.kind ?? 'insert'

// A record's change is in the queue and may already be on the server: no other change is
// written to the record until it is known (RecordBusy).
export const mayBeOnServer = (item: OutboxItem): boolean => item.state === 'syncing' || item.unconfirmed === true

// A change to a record whose earlier change may already be on the server.
export class RecordBusy extends Error {
  constructor() {
    super('an earlier change to this record may already be on the server')
    this.name = 'RecordBusy'
  }
}

// A refusal of the device's own to keep something: shown as the failure it is.
export class NotSaved extends Error {
  constructor(
    readonly reason: 'quota' | 'unavailable' | 'failed',
    readonly cause?: unknown,
  ) {
    super(`not saved on this device: ${reason}`)
    this.name = 'NotSaved'
  }
}

type Listener = () => void

export class OfflineStore {
  private listeners = new Set<Listener>()
  private closed = false

  private constructor(
    readonly user: number,
    private db: IDBDatabase,
  ) {}

  static databaseName(user: number): string {
    return `taskto-offline-${user}`
  }

  static async open(user: number): Promise<OfflineStore> {
    const name = OfflineStore.databaseName(user)
    const db = await openDatabase(name, SCHEMA_VERSION, MIGRATIONS)
    // A database whose purge began (it was found empty, and marked) but did not finish: it is empty
    // by construction, and goes before it is used again.
    let retired = false
    try {
      retired = (await runTx(db, [META], 'readonly', (tx) => tx.store(META).get(RETIRED))) !== undefined
    } catch {
      // Not read: used as it is.
    }
    if (!retired) return new OfflineStore(user, db)
    db.close()
    await deleteDatabase(name)
    return new OfflineStore(user, await openDatabase(name, SCHEMA_VERSION, MIGRATIONS))
  }

  // Called after every change made through this store (another tab's are seen the next time
  // its list is read).
  subscribe(fn: Listener): () => void {
    this.listeners.add(fn)
    return () => this.listeners.delete(fn)
  }

  private changed() {
    for (const fn of [...this.listeners]) fn()
  }

  private tx<R>(stores: string[], mode: IDBTransactionMode, fn: (tx: Tx) => Promise<R>): Promise<R> {
    if (this.closed) return Promise.reject(new Error('the offline database is closed'))
    return runTx(this.db, stores, mode, fn)
  }

  // A write that must be kept: any failure is a NotSaved, the quota's told apart.
  private async keep<R>(stores: string[], fn: (tx: Tx) => Promise<R>): Promise<R> {
    try {
      const out = await this.tx(stores, 'readwrite', fn)
      this.changed()
      return out
    } catch (e) {
      throw asNotSaved(e)
    }
  }

  // A write that is refused because the record is busy (the transaction is aborted, nothing is
  // written) says so, rather than that the device could not keep it.
  private async keepOrBusy<R>(stores: string[], fn: (tx: Tx) => Promise<R>): Promise<R> {
    try {
      return await this.keep(stores, fn)
    } catch (e) {
      if (e instanceof NotSaved && e.cause instanceof RecordBusy) throw e.cause
      throw e
    }
  }

  // --- downloads

  putDownload(d: Download): Promise<void> {
    return this.keep([DOWNLOADS], (tx) => tx.store<Download>(DOWNLOADS).put(d))
  }

  getDownload(scope: Scope): Promise<Download | undefined> {
    return this.tx([DOWNLOADS], 'readonly', (tx) => tx.store<Download>(DOWNLOADS).get(scopeKey(scope)))
  }

  listDownloads(): Promise<Download[]> {
    return this.tx([DOWNLOADS], 'readonly', (tx) => tx.store<Download>(DOWNLOADS).getAll())
  }

  // Removes an app's downloaded copy. What was queued for it stays: it is the person's work.
  removeDownload(scope: Scope): Promise<void> {
    return this.keep([DOWNLOADS], (tx) => tx.store(DOWNLOADS).delete(scopeKey(scope)))
  }

  // --- the outbox

  // Commits a new item and its photos in one transaction, and resolves only once it is durable.
  // A rejection is a NotSaved, and nothing of the item is kept: the caller shows the failure and
  // keeps the person's input.
  async enqueue(item: Omit<OutboxItem, 'seq'>, blobs: BlobRow[]): Promise<OutboxItem> {
    return this.keep([OUTBOX, BLOBS, META], async (tx) => {
      const meta = tx.store<{ key: string; value: number }>(META)
      // A database that was found empty and marked for deletion takes nothing more: the record is
      // reported as not saved, and the person keeps their input.
      if ((await meta.get(RETIRED)) !== undefined) throw new NotSaved('unavailable')
      const seq = ((await meta.get('seq'))?.value ?? 0) + 1
      await meta.put({ key: 'seq', value: seq })
      const saved = { ...item, seq }
      await tx.store<OutboxItem>(OUTBOX).put(saved)
      for (const b of blobs) await tx.store<BlobRow>(BLOBS).put(b)
      return saved
    })
  }

  // Records a change to a downloaded record (an update or a delete) in one transaction, with the
  // items already waiting for that record in view: `build` gets the record's item that the server
  // hasn't acknowledged, if there is one, and answers the item to keep: a new one, or that one
  // merged (it keeps its place in the queue). It may throw (RecordBusy). Resolves once durable.
  async changeRecord(
    scope: Scope,
    recordId: number,
    build: (prev: OutboxItem | undefined) => Omit<OutboxItem, 'seq'> | OutboxItem,
  ): Promise<OutboxItem> {
    return this.keepOrBusy([OUTBOX, META], async (tx) => {
      const meta = tx.store<{ key: string; value: number }>(META)
      if ((await meta.get(RETIRED)) !== undefined) throw new NotSaved('unavailable')
      const outbox = tx.store<OutboxItem>(OUTBOX)
      const waiting = (await outbox.getAllByIndex('byApp', scopeKey(scope)))
        .filter((i) => kindOf(i) !== 'insert' && i.record.id === recordId && i.state !== 'synced')
        .sort((a, b) => a.seq - b.seq)
      const next = build(waiting.at(-1))
      if ('seq' in next) {
        await outbox.put(next)
        return next
      }
      const seq = ((await meta.get('seq'))?.value ?? 0) + 1
      await meta.put({ key: 'seq', value: seq })
      const saved = { ...next, seq }
      await outbox.put(saved)
      return saved
    })
  }

  // The person's choice about an item that needs attention, applied in the item's own
  // transaction together with the downloaded copy it changes: `decide` sees the item and the copy
  // as they are stored now and answers what to keep (item: null removes it, with its photos), or
  // nothing to change none. Only an item that still needs attention, and is still the request
  // the person was shown (its operation key), is changed: a tab that moved it on meanwhile wins.
  // Answers whether it was applied.
  async resolve(
    id: string,
    operation: string,
    decide: (item: OutboxItem, copy: Download | undefined) => { item: OutboxItem | null; copy?: Download } | undefined,
  ): Promise<boolean> {
    return this.keep([OUTBOX, DOWNLOADS, BLOBS], async (tx) => {
      const outbox = tx.store<OutboxItem>(OUTBOX)
      const current = await outbox.get(id)
      if (current?.state !== 'attention' || current.operation !== operation) return false
      const downloads = tx.store<Download>(DOWNLOADS)
      const decided = decide(current, await downloads.get(current.scopeKey))
      if (!decided) return false
      if (decided.item === null) {
        await outbox.delete(id)
        const blobs = tx.store<BlobRow>(BLOBS)
        for (const b of await blobs.getAll()) if (b.item === id) await blobs.delete(b.id)
      } else {
        await outbox.put({ ...decided.item, updatedAt: new Date().toISOString() })
      }
      if (decided.copy) await downloads.put(decided.copy)
      return true
    })
  }

  getItem(id: string): Promise<OutboxItem | undefined> {
    return this.tx([OUTBOX], 'readonly', (tx) => tx.store<OutboxItem>(OUTBOX).get(id))
  }

  // Every item, in the order they were made.
  async listItems(scope?: Scope): Promise<OutboxItem[]> {
    const all = await this.tx([OUTBOX], 'readonly', (tx) =>
      scope
        ? tx.store<OutboxItem>(OUTBOX).getAllByIndex('byApp', scopeKey(scope))
        : tx.store<OutboxItem>(OUTBOX).getAll(),
    )
    return all.sort((a, b) => a.seq - b.seq)
  }

  // Lets go of the app's records that a fresh download holds, those it names by their operation:
  // the server has them, so the device's own copy (kept to show it until a download did) goes, and
  // with it, if it had not been told so, the wait to send it.
  //
  // A change the server acknowledged goes the same way: when the download holds the record with
  // its operation, or, for a change to a record the download no longer has (deleted, `present` is
  // the ids it holds, or null when it holds only the latest of more), when it is gone from it.
  async dropIncluded(
    scope: Scope,
    operations: Record<string, number>,
    present: ReadonlySet<number> | null = null,
  ): Promise<void> {
    for (const item of await this.listItems(scope)) {
      if (item.state !== 'synced') continue
      const included = item.operation in operations
      const gone = kindOf(item) !== 'insert' && present !== null && !present.has(item.record.id)
      if (included || gone) await this.removeItem(item.id)
    }
  }

  // Changes an item as it is now in the database, in one transaction: the change is a function of
  // the stored item, so two tabs never overwrite each other's progress with a stale copy.
  async updateItem(id: string, change: (item: OutboxItem) => OutboxItem | undefined): Promise<OutboxItem | undefined> {
    return this.keep([OUTBOX], async (tx) => {
      const store = tx.store<OutboxItem>(OUTBOX)
      const current = await store.get(id)
      if (!current) return undefined
      const next = change(current)
      if (!next) return current
      const saved = { ...next, updatedAt: new Date().toISOString() }
      await store.put(saved)
      return saved
    })
  }

  getBlob(id: string): Promise<Blob | undefined> {
    return this.tx([BLOBS], 'readonly', (tx) => tx.store<BlobRow>(BLOBS).get(id)).then((r) => r?.blob)
  }

  // Removes an item with its photos: a discard, or a synced one that is let go.
  async removeItem(id: string): Promise<void> {
    await this.keep([OUTBOX, BLOBS], async (tx) => {
      await tx.store(OUTBOX).delete(id)
      const blobs = tx.store<BlobRow>(BLOBS)
      for (const b of await blobs.getAll()) if (b.item === id) await blobs.delete(b.id)
    })
  }

  // Drops the photos of an item the server has: they are no longer needed.
  dropBlobs(item: string): Promise<void> {
    return this.keep([BLOBS], async (tx) => {
      const blobs = tx.store<BlobRow>(BLOBS)
      for (const b of await blobs.getAll()) if (b.item === item) await blobs.delete(b.id)
    })
  }

  // How many items the server hasn't acknowledged: what signing out would lose.
  async unsynced(): Promise<OutboxItem[]> {
    return (await this.listItems()).filter((i) => i.state !== 'synced')
  }

  // The one step that decides what becomes of a database that is let go of, in a single
  // transaction, so a record committed meanwhile is never lost between a count and a deletion:
  //   - nothing unsent (or only what `approved` names: records the person was shown and agreed to
  //     lose): everything is cleared and the database is marked as being deleted, so that nothing
  //     can be written to it after this; the answer is none, and the caller deletes it (`destroy`);
  //   - otherwise: only the records the server hasn't acknowledged are kept, with their photos
  //     (everything downloaded goes, and so do the records that were sent and what they hold of the
  //     server's data), and the answer is which; the unsent are the person's own work, which
  //     they send by signing in as the account again, or discard. (`seal: false`, for an account
  //     still signed in, leaves the database exactly as it is instead.)
  async retireOrKeepUnsent(approved: ReadonlySet<string> = new Set(), seal = true): Promise<string[]> {
    return this.keep([DOWNLOADS, OUTBOX, BLOBS, META], async (tx) => {
      const outbox = tx.store<OutboxItem>(OUTBOX)
      const items = await outbox.getAll()
      const unsent = items.filter((i) => i.state !== 'synced')
      const everyApproved = unsent.every((i) => approved.has(i.id))
      if (!everyApproved && !seal) return unsent.map((i) => i.id)
      const downloads = tx.store<Download>(DOWNLOADS)
      for (const d of await downloads.getAll()) await downloads.delete(d.key)
      const blobs = tx.store<BlobRow>(BLOBS)
      if (everyApproved) {
        for (const i of items) await outbox.delete(i.id)
        for (const b of await blobs.getAll()) await blobs.delete(b.id)
        await tx.store<{ key: string; value: number }>(META).put({ key: RETIRED, value: 1 })
        return []
      }
      const gone = new Set<string>()
      for (const i of items) {
        if (i.state !== 'synced') continue
        gone.add(i.id)
        await outbox.delete(i.id)
      }
      for (const b of await blobs.getAll()) if (gone.has(b.item)) await blobs.delete(b.id)
      return unsent.map((i) => i.id)
    })
  }

  // --- the whole database

  close() {
    this.closed = true
    this.db.close()
  }

  // Deletes everything of this user's on this device, the database itself.
  async destroy(): Promise<void> {
    this.close()
    await deleteDatabase(OfflineStore.databaseName(this.user))
    this.changed()
  }

  static destroyFor(user: number): Promise<void> {
    return deleteDatabase(OfflineStore.databaseName(user))
  }
}

function asNotSaved(e: unknown): NotSaved {
  if (e instanceof NotSaved) return e
  const name = (e as { name?: string } | null)?.name
  if (name === 'QuotaExceededError' || (e as { code?: number } | null)?.code === 22) return new NotSaved('quota', e)
  if (name === 'StorageUnavailable' || name === 'InvalidStateError' || name === 'SecurityError')
    return new NotSaved('unavailable', e)
  return new NotSaved('failed', e)
}
