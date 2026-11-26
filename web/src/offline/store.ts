// What the device keeps for offline use, one IndexedDB database per signed-in user:
// `taskto-offline-<user id>`. Nothing of one account is ever in another's database, so an
// account signing in on the same browser can't read, write or even open the last one's, and
// signing out (or discarding) deletes the database itself (docs/offline.md).
//
// Four stores:
//   downloads  an app's downloaded release and records, one per (workspace, space, app)
//   outbox     the records made offline and waiting for the server, with where they go
//   blobs      the photos of queued records, which stay until the server has them
//   meta       small facts (the sequence for new items, which tab is the offline writer, the
//              generation of the downloaded copies)
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
  // Which copy this is: raised each time a copy is put or removed (`putDownload`,
  // `removeDownload`), and never by a change made to the copy it is (a choice about a conflict).
  // What was read from the server for the copy held at some point is written into it only while
  // this is still the copy it was read for (`resolve`: CopyChanged).
  generation?: number
  // The order downloads began in (`beginDownload`): a download is never written over a copy that
  // came from one that began after it (`putDownload`). Copies put without one have none.
  ticket?: number
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
  // Counts every change made to the item, up from 1: each one is a compare-and-set in a single
  // transaction against the version its maker read (`changeRecord`, `resolve`, `removeItem`,
  // `retireOrKeepUnsent`), so a screen, a confirmation or a choice that was made on an older
  // version is refused (ItemChanged) and never applied to what another tab made of the item.
  // The engine's own writes (the claim on an item and its renewal, a photo's progress, what a send's
  // answer made of it) leave it: whatever a person is shown of them comes after an action of theirs
  // (a retry, a choice) that raised it. Items of an earlier build have none: 0.
  version: number
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
  // For an update the server acknowledged: the record as the server answered it (an item: its id,
  // `_revision` and fields), when the answer said so. The record is shown from it: the server's
  // revision is only ever paired with the server's values, and a later change to the record is
  // based on that revision. An answer that didn't say leaves none, and the record is shown as the
  // person left it, with no revision to base a change on.
  ack?: Record<string, unknown>
  // Set once a send was started and the server's answer wasn't a definite one (no answer, 5xx,
  // the page was closed): the change may be on the server. Such an item takes no further edit to
  // its record, and its key is never reused for another request. Only a definite answer clears
  // it: one that says the session ended says nothing of what an earlier send did.
  unconfirmed?: boolean
  // Whether `unconfirmed` was set before the send that is in flight (which sets it): what an
  // answer that isn't a definite one goes back to.
  unconfirmedBefore?: boolean
  // How many times a send claimed the item. A confirmation to discard it is about the item as it
  // was, and is refused once another send has been made (it may be on the server now).
  sends?: number
  // The person cleared it from the list once the server had it: it stays, to keep showing the
  // record as the server has it, until a download that holds the change makes it go.
  cleared?: boolean
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
  // once, with or without Web Locks. A lease that has run out is anyone's to take. `attempt` is
  // the token of the send the claim was made for: only the send that holds it may write the answer
  // it got (a late answer to a send that was superseded is dropped).
  lease?: { owner: string; until: number; attempt?: string }
}

export type BlobRow = { id: string; item: string; blob: Blob }

// What an item does: an item that says nothing was made by version 1 of the database, an insert.
export const kindOf = (item: Pick<OutboxItem, 'kind'>): ChangeKind => item.kind ?? 'insert'

export const versionOf = (item: Partial<Pick<OutboxItem, 'version'>>): number => item.version ?? 0

// An item as someone was shown it: what a confirmation, an approval or a choice carries, and is
// checked against in the transaction that applies it.
export type Seen = { version: number; operation: string }
export const seenOf = (item: Pick<OutboxItem, 'operation'> & Partial<Pick<OutboxItem, 'version'>>): Seen => ({
  version: versionOf(item),
  operation: item.operation,
})
const isSeen = (item: OutboxItem, seen: Seen) => versionOf(item) === seen.version && item.operation === seen.operation

// An unsent item as the person was shown it, for what they approve to lose.
export type UnsentRef = Seen & { id: string }
export type Approval = ReadonlyMap<string, Seen>
export const approvalOf = (refs: readonly UnsentRef[]): Approval => new Map(refs.map((r) => [r.id, r]))

// A record's change is in the queue and may already be on the server: no other change is
// written to the record until it is known (RecordBusy).
export const mayBeOnServer = (item: OutboxItem): boolean => item.state === 'syncing' || item.unconfirmed === true

// A write the store refuses, rather than fails: it is not about the device's storage, so it is
// told as what it is and never as "not saved".
export class Refused extends Error {}

// A change to a record whose earlier change may already be on the server.
export class RecordBusy extends Refused {
  constructor() {
    super('an earlier change to this record may already be on the server')
    this.name = 'RecordBusy'
  }
}

// What a confirmation, an approval or a choice was made on is not what is stored now: another tab
// changed the item meanwhile, and the person is asked again about what is there.
export class ItemChanged extends Refused {
  constructor() {
    super('this changed in another tab: review it again')
    this.name = 'ItemChanged'
  }
}

// A local change made on a screen that is not drawn from what the queue holds for its record (the
// same base revision, the same release): never mixed with it. The person reopens the record.
export class RecordChanged extends Refused {
  constructor() {
    super('this record changed: reopen it')
    this.name = 'RecordChanged'
  }
}

// A change to existing records, a choice about one, or a discard, made by a tab that is not the
// offline writer: only one tab of an account at a time makes them (docs/offline.md, "One writer").
export class NotWriter extends Refused {
  constructor() {
    super('changes are being made in another tab')
    this.name = 'NotWriter'
  }
}

// What a choice read from the server was read for a downloaded copy that is not the one held now (a
// revalidation or a new download replaced it meanwhile): nothing of it is written into the copy.
export class CopyChanged extends Refused {
  constructor() {
    super('the downloaded copy changed meanwhile: review it again')
    this.name = 'CopyChanged'
  }
}

// A download that was overtaken: a download that began after it has been written (or the copy was
// removed after it began), so what it holds is older than what the device has and is not written.
export class StaleDownload extends Refused {
  constructor() {
    super('a newer download is held')
    this.name = 'StaleDownload'
  }
}

// The account signed in on this browser is not the one this database is for: nothing is written.
export class AccountLapsed extends Refused {
  constructor() {
    super('the account signed in here is not this one')
    this.name = 'AccountLapsed'
  }
}

// What the item was when a person was shown it, for what only that state may be undone (a discard).
// `sends` counts the times a send claimed the item: one that went out and came back to the queue
// (same version, same key) is not the item the person was asked about, since it may be on the
// server now.
// `lease` is who held the engine's claim on it (empty for none): a claim made and given up since
// is not the item the person was shown either.
export type SeenAs = Seen & { state: OutboxState; sends?: number; lease?: string }
const leaseOf = (item: Pick<OutboxItem, 'lease'>): string =>
  item.lease ? `${item.lease.owner}/${item.lease.attempt ?? ''}` : ''
export const seenAs = (item: OutboxItem): SeenAs => ({
  ...seenOf(item),
  state: item.state,
  sends: item.sends ?? 0,
  lease: leaseOf(item),
})
const isSeenAs = (item: OutboxItem, seen: SeenAs) =>
  isSeen(item, seen) &&
  item.state === seen.state &&
  (item.sends ?? 0) === (seen.sends ?? 0) &&
  leaseOf(item) === (seen.lease ?? '')

// Whether there is any sign of an earlier send (a claim, one that gave no answer, or, on entries
// from before `sends` was counted, a recorded attempt): the server may already have the record, so
// what is said of removing it from this device never promises it was not sent.
export const mayHaveBeenSent = (item: Pick<OutboxItem, 'sends' | 'lease' | 'unconfirmed' | 'attempts'>): boolean =>
  (item.sends ?? 0) > 0 || (item.attempts ?? 0) > 0 || item.lease !== undefined || item.unconfirmed === true

// A record's queue as a screen was drawn from it: what an enqueue checks again in its transaction.
export const queueSignature = (items: readonly OutboxItem[]): string =>
  items
    .filter((i) => kindOf(i) !== 'insert')
    .map((i) => `${i.id}:${versionOf(i)}:${i.state}:${i.unconfirmed ? 1 : 0}`)
    .sort()
    .join(',')

// The meta row that says which tab holds the right to change existing records, and until when.
export const WRITER = 'writer'
// `via`: how the holder got it, by a Web Lock it holds (any other tab with Web Locks is waiting for
// that lock, so a row left by a holder whose lock is gone is free to a tab that holds the lock) or
// by the lease alone (free only once `until` has passed). Whoever holds it renews `until`.
export type WriterRow = { key: typeof WRITER; owner: string; until: number; via?: 'lock' | 'lease' }

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

// The channel the tabs of an account tell each other on (a BroadcastChannel's shape).
export type TabChannel = Pick<BroadcastChannel, 'postMessage' | 'close' | 'onmessage'>

// A refusal that says the change was made on a view of the queue that is not what is stored.
const staleView = (e: unknown) =>
  e instanceof ItemChanged || e instanceof NotWriter || e instanceof RecordChanged || e instanceof CopyChanged

export class OfflineStore {
  private listeners = new Set<Listener>()
  private closed = false
  private tabs: TabChannel | null = null
  // Told apart from the other tabs' on the channel.
  private readonly tabId = Math.random().toString(36).slice(2)
  // The tab's identity as the offline writer, once the writer model is attached (writer.ts): the
  // changes only the writer makes are written only while the database says it is this one. Without
  // one nothing is checked (a tool or a test that has no competing tabs).
  private writer: string | undefined
  // The clock the writer's lease is compared with.
  private clock: () => number = Date.now
  // Whether the account signed in here is this database's: asked in the transaction, right before
  // each write.
  private account: () => boolean = () => true

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

  // Called after every change made through this store, and when another tab of the account says it
  // changed the database (`connectTabs`), or a refusal showed that what is on screen is stale
  // (`refresh`): the lists read again.
  subscribe(fn: Listener): () => void {
    this.listeners.add(fn)
    return () => this.listeners.delete(fn)
  }

  private notify() {
    for (const fn of [...this.listeners]) fn()
  }

  private changed() {
    this.notify()
    try {
      this.tabs?.postMessage({ from: this.tabId })
    } catch {
      // The other tabs are not told: they read again the next time they are asked to.
    }
  }

  // Joins the channel the tabs of this account tell each other on that the database changed: a
  // tab's write is announced, and a tab that hears it tells its own subscribers (and does not
  // announce it again). Without a channel (a browser that has none) a tab sees another's changes
  // when its list is read again for another reason (a refusal, the writer role coming back).
  connectTabs(channel: (name: string) => TabChannel | null) {
    this.disconnectTabs()
    try {
      const chan = channel(`taskto-offline-changes-${this.user}`)
      if (!chan) return
      chan.onmessage = (e: MessageEvent) => {
        const msg = e.data as { from?: string } | null
        if (msg?.from !== this.tabId && !this.closed) this.notify()
      }
      this.tabs = chan
    } catch {
      this.tabs = null
    }
  }

  private disconnectTabs() {
    try {
      this.tabs?.close()
    } catch {
      // Closed already.
    }
    this.tabs = null
  }

  // What is on screen was found to be stale (a change was refused as made on an older view of the
  // item): every list of this tab reads the database again, at once.
  refresh() {
    if (!this.closed) this.notify()
  }

  // From now on the changes only the offline writer makes (a change to a record, a choice, a
  // discard) are written only while `owner` is the one the database names (`claimWriter`).
  attachWriter(owner: string | undefined, now: () => number = Date.now) {
    this.writer = owner
    this.clock = now
  }

  // Asks, for every write, whether the account signed in here is this database's. Asked inside the
  // transaction that writes, after what it read and before what it puts, so a sign-in that came
  // while an answer was on its way can't be written under the account that has left.
  requireAccount(check: () => boolean) {
    this.account = check
  }

  private assertAccount() {
    if (!this.account()) throw new AccountLapsed()
  }

  // Inside a transaction that also reads META: the write is refused unless this tab is the writer,
  // and its lease has not run out: until another tab replaces the row, an expired one is a right
  // nobody renewed (the tab froze), and another tab may be about to take it.
  private async assertWriter(tx: Tx) {
    this.assertAccount()
    if (this.writer === undefined) return
    const row = await tx.store<WriterRow>(META).get(WRITER)
    if (row?.owner !== this.writer || row.until <= this.clock()) throw new NotWriter()
  }

  // The writer row, in one transaction (a compare-and-set: the database's word is the one source of
  // truth for who the writer is, whatever the tabs use to take turns): taken when `free` says the
  // row (if any) is free to take, or when it is already `owner`'s, which renews it. Answers whether
  // `owner` holds it now.
  async claimWriter(
    owner: string,
    until: number,
    free: (row: WriterRow | undefined) => boolean,
    via: WriterRow['via'] = 'lease',
  ): Promise<boolean> {
    return this.tx([META], 'readwrite', async (tx) => {
      const meta = tx.store<WriterRow | { key: string; value: number }>(META)
      if ((await meta.get(RETIRED)) !== undefined) return false
      const row = (await meta.get(WRITER)) as WriterRow | undefined
      if (row?.owner !== owner && !free(row)) return false
      await meta.put({ key: WRITER, owner, until, via })
      return true
    })
  }

  // The writer row is let go, if it is `owner`'s.
  async releaseWriter(owner: string): Promise<void> {
    await this.tx([META], 'readwrite', async (tx) => {
      const meta = tx.store<WriterRow>(META)
      if ((await meta.get(WRITER))?.owner === owner) await meta.delete(WRITER)
    })
  }

  // Whether `owner` holds the writer row now.
  async writerIs(owner: string): Promise<boolean> {
    return (await this.tx([META], 'readonly', (tx) => tx.store<WriterRow>(META).get(WRITER)))?.owner === owner
  }

  private tx<R>(stores: string[], mode: IDBTransactionMode, fn: (tx: Tx) => Promise<R>): Promise<R> {
    if (this.closed) return Promise.reject(new Error('the offline database is closed'))
    return runTx(this.db, stores, mode, fn)
  }

  // A write that must be kept: any failure is a NotSaved, the quota's told apart.
  private async keep<R>(stores: string[], fn: (tx: Tx) => Promise<R>): Promise<R> {
    try {
      const out = await this.tx(stores, 'readwrite', async (tx) => {
        this.assertAccount()
        const result = await fn(tx)
        // Asked again after everything the transaction waited for: a sign-in that came meanwhile
        // aborts it, and nothing is written under the account that has left.
        this.assertAccount()
        return result
      })
      this.changed()
      return out
    } catch (e) {
      // A refusal (the transaction is aborted, nothing is written) says so, rather than that the
      // device could not keep it. One made on a stale view also makes every list of this tab read
      // again, so the person's next attempt acts on what is really there.
      if (e instanceof Refused) {
        if (staleView(e)) this.refresh()
        throw e
      }
      throw asNotSaved(e)
    }
  }

  // --- downloads

  // Keeps a copy as a new one: its generation is a new number, whatever the copy given says.
  //
  // With a `ticket` (from `beginDownload`, taken before the request was made) the copy is written
  // only if no download that began later is held, and none was removed since this one began: a
  // download that was overtaken (a newer one was written, and the records it no longer holds were
  // let go with it) never replaces the newer, so a deleted record can't come back from an older
  // answer. Decided in the writing transaction (StaleDownload).
  putDownload(d: Download, ticket?: number): Promise<void> {
    return this.keep([DOWNLOADS, META], async (tx) => {
      if (ticket !== undefined) {
        const held = await tx.store<Download>(DOWNLOADS).get(d.key)
        const floor = (await tx.store<{ key: string; value: number }>(META).get(floorKey(d.key)))?.value ?? 0
        if (ticket <= floor || (held?.ticket ?? 0) > ticket) throw new StaleDownload()
        d.ticket = ticket
      }
      d.generation = await nextGeneration(tx)
      await tx.store<Download>(DOWNLOADS).put(d)
    })
  }

  // The order downloads began in, one counter for the database (so it orders those of every tab):
  // taken before a download's request is made.
  beginDownload(): Promise<number> {
    return this.keep([META], async (tx) => nextCounter(tx, 'downloadTicket'))
  }

  // The generation of the copy held for a scope, or null when none is.
  async downloadGeneration(scope: Scope): Promise<number | null> {
    return (await this.getDownload(scope))?.generation ?? null
  }

  getDownload(scope: Scope): Promise<Download | undefined> {
    return this.tx([DOWNLOADS], 'readonly', (tx) => tx.store<Download>(DOWNLOADS).get(scopeKey(scope)))
  }

  listDownloads(): Promise<Download[]> {
    return this.tx([DOWNLOADS], 'readonly', (tx) => tx.store<Download>(DOWNLOADS).getAll())
  }

  // Removes an app's downloaded copy. What was queued for it stays: it is the person's work.
  removeDownload(scope: Scope): Promise<void> {
    return this.keep([DOWNLOADS, META], async (tx) => {
      await nextGeneration(tx)
      // A download that began before this removal doesn't bring the copy back (`putDownload`).
      const began = ((await tx.store<{ key: string; value: number }>(META).get('downloadTicket'))?.value ?? 0) as number
      await tx.store(META).put({ key: floorKey(scopeKey(scope)), value: began })
      await tx.store(DOWNLOADS).delete(scopeKey(scope))
    })
  }

  // --- the outbox

  // Commits a new item and its photos in one transaction, and resolves only once it is durable.
  // A rejection is a NotSaved, and nothing of the item is kept: the caller shows the failure and
  // keeps the person's input.
  async enqueue(item: Omit<OutboxItem, 'seq' | 'version'>, blobs: BlobRow[]): Promise<OutboxItem> {
    return this.keep([OUTBOX, BLOBS, META], async (tx) => {
      const meta = tx.store<{ key: string; value: number }>(META)
      // A database that was found empty and marked for deletion takes nothing more: the record is
      // reported as not saved, and the person keeps their input.
      if ((await meta.get(RETIRED)) !== undefined) throw new NotSaved('unavailable')
      const seq = ((await meta.get('seq'))?.value ?? 0) + 1
      await meta.put({ key: 'seq', value: seq })
      const saved = { ...item, seq, version: 1 }
      await tx.store<OutboxItem>(OUTBOX).put(saved)
      for (const b of blobs) await tx.store<BlobRow>(BLOBS).put(b)
      return saved
    })
  }

  // Records a change to a downloaded record (an update or a delete) in one transaction, with every
  // item already made for that record in view: `build` gets the record's item that the server
  // hasn't acknowledged, if there is one, and answers the item to keep: a new one, or that one
  // merged (it keeps its place in the queue, and its version goes up). Refused, and nothing is
  // written: by a tab that isn't the writer (NotWriter); when any item of the record may already be
  // on the server (RecordBusy: sent and not answered for good); when any is a deletion, or the
  // record's queue is not what `drawnFrom` (a `queueSignature`) says the screen was drawn from, an
  // item having been added, answered or changed meanwhile (RecordChanged); and by what `build`
  // throws. Resolves once durable.
  async changeRecord(
    scope: Scope,
    recordId: number,
    build: (prev: OutboxItem | undefined) => Omit<OutboxItem, 'seq' | 'version'> | OutboxItem,
    drawnFrom?: string,
  ): Promise<OutboxItem> {
    return this.keep([OUTBOX, META], async (tx) => {
      const meta = tx.store<{ key: string; value: number }>(META)
      if ((await meta.get(RETIRED)) !== undefined) throw new NotSaved('unavailable')
      await this.assertWriter(tx)
      const outbox = tx.store<OutboxItem>(OUTBOX)
      const all = (await outbox.getAllByIndex('byApp', scopeKey(scope)))
        .filter((i) => kindOf(i) !== 'insert' && i.record.id === recordId)
        .sort((a, b) => a.seq - b.seq)
      if (all.some(mayBeOnServer)) throw new RecordBusy()
      if (all.some((i) => kindOf(i) === 'delete')) throw new RecordChanged()
      if (drawnFrom !== undefined && queueSignature(all) !== drawnFrom) throw new RecordChanged()
      const prev = all.filter((i) => i.state !== 'synced').at(-1)
      const next = build(prev)
      if ('seq' in next) {
        // The item as this transaction read it, one version on.
        const merged = { ...next, version: versionOf(prev as OutboxItem) + 1 }
        await outbox.put(merged)
        return merged
      }
      const seq = ((await meta.get('seq'))?.value ?? 0) + 1
      await meta.put({ key: 'seq', value: seq })
      const saved = { ...next, seq, version: 1 }
      await outbox.put(saved)
      return saved
    })
  }

  // The person's choice about an item that needs attention, applied in the item's own
  // transaction together with the downloaded copy it changes: `decide` sees the item and the copy
  // as they are stored now and answers what to keep (item: null removes it, with its photos), or
  // nothing to change none. Only an item that still needs attention, and is still the item the
  // person was shown (its version and its operation key), is changed: a tab that moved it on
  // meanwhile wins, and what the person chose is refused (ItemChanged). Answers whether it was
  // applied (`decide` may find nothing to change). Only the offline writer chooses (NotWriter). With
  // `copy` (the generation of the downloaded copy, or null for none, as it was when the server was
  // read), a copy that is not that one any more (a revalidation or a download replaced it
  // meanwhile) refuses the choice (CopyChanged): what was read is not written into another copy.
  async resolve(
    id: string,
    seen: Seen,
    decide: (item: OutboxItem, copy: Download | undefined) => { item: OutboxItem | null; copy?: Download } | undefined,
    { copy: readFor }: { copy?: number | null } = {},
  ): Promise<boolean> {
    return this.keep([OUTBOX, DOWNLOADS, BLOBS, META], async (tx) => {
      await this.assertWriter(tx)
      const outbox = tx.store<OutboxItem>(OUTBOX)
      const current = await outbox.get(id)
      if (current?.state !== 'attention' || !isSeen(current, seen)) throw new ItemChanged()
      const downloads = tx.store<Download>(DOWNLOADS)
      const held = await downloads.get(current.scopeKey)
      if (readFor !== undefined && (held?.generation ?? null) !== readFor) throw new CopyChanged()
      const decided = decide(current, held)
      if (!decided) return false
      if (decided.item === null) {
        await outbox.delete(id)
        const blobs = tx.store<BlobRow>(BLOBS)
        for (const b of await blobs.getAll()) if (b.item === id) await blobs.delete(b.id)
      } else {
        await outbox.put({ ...decided.item, version: versionOf(current) + 1, updatedAt: new Date().toISOString() })
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
  // the stored item, so two tabs never overwrite each other's progress with a stale copy. The
  // version goes up with the change, unless `bump` is false: the engine's own writes (the claim on
  // an item, its renewal, a photo's progress, what a send's answer made of it) are not changes
  // anyone was shown; whatever of them a person sees next, they see after an action of theirs
  // (a retry, a choice) that went through a bump.
  async updateItem(
    id: string,
    change: (item: OutboxItem) => OutboxItem | undefined,
    { bump = true, guarded = false, seen }: { bump?: boolean; guarded?: boolean; seen?: SeenAs } = {},
  ): Promise<OutboxItem | undefined> {
    return this.keep(guarded ? [OUTBOX, META] : [OUTBOX], async (tx) => {
      if (guarded) await this.assertWriter(tx)
      const store = tx.store<OutboxItem>(OUTBOX)
      const current = await store.get(id)
      // A person's press carries the item as it was shown (`seen`): one that moved on since (sent,
      // claimed, edited, settled), or is gone, is refused, and nothing is applied.
      if (seen && (!current || !isSeenAs(current, seen))) throw new ItemChanged()
      if (!current) return undefined
      const next = change(current)
      if (!next) return current
      // Right before the write, after everything this transaction waited for.
      this.assertAccount()
      const saved = { ...next, version: versionOf(current) + (bump ? 1 : 0), updatedAt: new Date().toISOString() }
      await store.put(saved)
      return saved
    })
  }

  // The engine's claim on an item, in its own transaction, with the queue of the item's app as that
  // transaction reads it: `claim` decides on the item and the queue together, so a condition about
  // the other items (an earlier change to the same record that has no answer for good) is checked
  // atomically with the claim, never on a list read before. The version is left as it is (a claim
  // is not a change anyone was shown).
  async claimItem(
    id: string,
    claim: (item: OutboxItem, queue: readonly OutboxItem[]) => OutboxItem | undefined,
  ): Promise<OutboxItem | undefined> {
    return this.keep([OUTBOX], async (tx) => {
      const store = tx.store<OutboxItem>(OUTBOX)
      const current = await store.get(id)
      if (!current) return undefined
      const queue = await store.getAllByIndex('byApp', current.scopeKey)
      const next = claim(current, queue)
      if (!next) return current
      this.assertAccount()
      const saved = { ...next, version: versionOf(current), updatedAt: new Date().toISOString() }
      await store.put(saved)
      return saved
    })
  }

  getBlob(id: string): Promise<Blob | undefined> {
    return this.tx([BLOBS], 'readonly', (tx) => tx.store<BlobRow>(BLOBS).get(id)).then((r) => r?.blob)
  }

  // The person's discard of an item that was never acknowledged, with its photos: only the offline
  // writer does it (NotWriter), and only the item as it was shown when the question was asked, its
  // version, its operation and its state: one that changed since, or is gone, or that the server
  // has acknowledged or may be sending, is kept (ItemChanged). A synced entry is never discarded:
  // it is how the record is shown until a download has it.
  async discardItem(id: string, seen: SeenAs): Promise<void> {
    await this.keep([OUTBOX, BLOBS, META], async (tx) => {
      await this.assertWriter(tx)
      const outbox = tx.store<OutboxItem>(OUTBOX)
      const current = await outbox.get(id)
      if (!current || !isSeenAs(current, seen)) throw new ItemChanged()
      if (current.state === 'synced' || current.state === 'syncing') throw new ItemChanged()
      await outbox.delete(id)
      const blobs = tx.store<BlobRow>(BLOBS)
      for (const b of await blobs.getAll()) if (b.item === id) await blobs.delete(b.id)
    })
  }

  // Removes an item with its photos: a synced one that a download holds is let go (the engine's and
  // the download's, not a person's discard: `discardItem`).
  async removeItem(id: string): Promise<void> {
    await this.keep([OUTBOX, BLOBS], async (tx) => {
      const outbox = tx.store<OutboxItem>(OUTBOX)
      await outbox.delete(id)
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
  //     lose, each as it was shown, by its version and its operation: an item that changed since,
  //     a newer edit coalesced into it, is not what they agreed to lose): everything is cleared and the database is marked as being deleted, so that nothing
  //     can be written to it after this; the answer is none, and the caller deletes it (`destroy`);
  //   - otherwise: only the records the server hasn't acknowledged are kept, with their photos
  //     (everything downloaded goes, and so do the records that were sent and what they hold of the
  //     server's data), and the answer is which; the unsent are the person's own work, which
  //     they send by signing in as the account again, or discard. (`seal: false`, for an account
  //     still signed in, leaves the database exactly as it is instead.)
  async retireOrKeepUnsent(approved: Approval = new Map(), seal = true): Promise<UnsentRef[]> {
    return this.keep([DOWNLOADS, OUTBOX, BLOBS, META], async (tx) => {
      const outbox = tx.store<OutboxItem>(OUTBOX)
      const items = await outbox.getAll()
      const unsent = items.filter((i) => i.state !== 'synced')
      const everyApproved = unsent.every((i) => {
        const shown = approved.get(i.id)
        return shown !== undefined && isSeen(i, shown)
      })
      const refs = unsent.map((i): UnsentRef => ({ id: i.id, ...seenOf(i) }))
      if (!everyApproved && !seal) return refs
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
      return refs
    })
  }

  // --- the whole database

  close() {
    this.closed = true
    this.disconnectTabs()
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

// The next number for a copy of a download: one counter for the database, so a copy put after one
// was removed is never mistaken for it.
async function nextGeneration(tx: Tx): Promise<number> {
  return nextCounter(tx, 'downloadGen')
}

async function nextCounter(tx: Tx, key: string): Promise<number> {
  const meta = tx.store<{ key: string; value: number }>(META)
  const next = ((await meta.get(key))?.value ?? 0) + 1
  await meta.put({ key, value: next })
  return next
}

// Where the last ticket issued is noted when an app's copy is removed.
const floorKey = (key: string) => `downloadFloor:${key}`
