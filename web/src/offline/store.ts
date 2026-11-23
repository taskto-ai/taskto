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

export const SCHEMA_VERSION = 1

export const DOWNLOADS = 'downloads'
export const OUTBOX = 'outbox'
export const BLOBS = 'blobs'
export const META = 'meta'

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
  // About how big the copy is, for the storage the person is shown.
  bytes: number
}

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
// failing.
export type AttentionReason = 'refused' | 'access' | 'archived' | 'outdated' | 'gave_up' | 'photo'

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
  // The record it makes: a negative local id until the server has it; its revision is unknown
  // until then (an insert has none).
  record: { id: number; revision: number | null }
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
}

export type BlobRow = { id: string; item: string; blob: Blob }

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
    return new OfflineStore(user, await openDatabase(OfflineStore.databaseName(user), SCHEMA_VERSION, MIGRATIONS))
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
      const seq = ((await meta.get('seq'))?.value ?? 0) + 1
      await meta.put({ key: 'seq', value: seq })
      const saved = { ...item, seq }
      await tx.store<OutboxItem>(OUTBOX).put(saved)
      for (const b of blobs) await tx.store<BlobRow>(BLOBS).put(b)
      return saved
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
