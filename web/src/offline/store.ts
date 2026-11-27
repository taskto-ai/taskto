import type { OfflineApp } from '../api'
import { deleteDatabase, type Migration, openDatabase, runTx, StorageUnavailable, type Tx } from './idb'

export const SCHEMA_VERSION = 2

export const DOWNLOADS = 'downloads'
export const OUTBOX = 'outbox'
export const BLOBS = 'blobs'
export const META = 'meta'
const RETIRED = 'retired'

export const MIGRATIONS: Record<number, Migration> = {
  1: (db) => {
    db.createObjectStore(DOWNLOADS, { keyPath: 'key' })
    const outbox = db.createObjectStore(OUTBOX, { keyPath: 'id' })
    outbox.createIndex('byApp', 'scopeKey')
    outbox.createIndex('byState', 'state')
    db.createObjectStore(BLOBS, { keyPath: 'id' })
    db.createObjectStore(META, { keyPath: 'key' })
  },
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

export type Download = Scope & {
  key: string
  user: number
  name: string
  icon?: string
  release: number
  definition: OfflineApp['definition']
  messages: unknown[]
  scopeOf: 'all' | 'own'
  records: number
  total: number
  truncated: boolean
  canInsert: boolean
  capturedAt: string
  operations: Record<string, number>
  modifiable?: number[]
  bytes: number
  generation?: number
  ticket?: number
}

export type ChangeKind = 'insert' | 'update' | 'delete'

export type OutboxState = 'draft' | 'queued' | 'syncing' | 'synced' | 'attention'

export type Attachment = {
  localId: string
  field: string
  blobId: string
  size: number
  type: string
  state: 'pending' | 'uploading' | 'uploaded'
  uploadId?: string
  serverId?: string
}

export type ItemError = { status?: number; code?: string; params?: Record<string, unknown>; message: string }

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

export type Problem = { field: string; code: string; params?: Record<string, unknown> }

export type Conflict = { current: Record<string, unknown> | null }

export type OutboxItem = {
  id: string
  seq: number
  version: number
  user: number
  scopeKey: string
  workspace: number
  space: number
  app: number
  base: string
  release: number
  schema: number
  action: {
    name: string
    surfaceId: string
    sourceComponentId: string
    timestamp: string
    context: Record<string, unknown>
  }
  operation: string
  kind: ChangeKind
  record: { id: number; revision: number | null }
  original?: Record<string, unknown>
  ack?: Record<string, unknown>
  unconfirmed?: boolean
  unconfirmedBefore?: boolean
  sends?: number
  cleared?: boolean
  conflict?: Conflict
  problems?: Problem[]
  restore?: { revision: number }
  attachments: Attachment[]
  state: OutboxState
  attempts: number
  nextAttemptAt?: number
  attention?: AttentionReason
  error?: ItemError
  createdAt: string
  updatedAt: string
  syncedAt?: string
  lease?: { owner: string; until: number; attempt?: string }
}

export type BlobRow = { id: string; item: string; blob: Blob }

export const kindOf = (item: Pick<OutboxItem, 'kind'>): ChangeKind => item.kind ?? 'insert'

export const versionOf = (item: Partial<Pick<OutboxItem, 'version'>>): number => item.version ?? 0

export type Seen = { version: number; operation: string }
export const seenOf = (item: Pick<OutboxItem, 'operation'> & Partial<Pick<OutboxItem, 'version'>>): Seen => ({
  version: versionOf(item),
  operation: item.operation,
})
const isSeen = (item: OutboxItem, seen: Seen) => versionOf(item) === seen.version && item.operation === seen.operation

export type UnsentRef = Seen & { id: string }
export type Approval = ReadonlyMap<string, Seen>
export const approvalOf = (refs: readonly UnsentRef[]): Approval => new Map(refs.map((r) => [r.id, r]))

export const mayBeOnServer = (item: OutboxItem): boolean => item.state === 'syncing' || item.unconfirmed === true

export class Refused extends Error {}

export class RecordBusy extends Refused {
  constructor() {
    super('an earlier change to this record may already be on the server')
    this.name = 'RecordBusy'
  }
}

export class ItemChanged extends Refused {
  constructor() {
    super('this changed in another tab: review it again')
    this.name = 'ItemChanged'
  }
}

export class RecordChanged extends Refused {
  constructor() {
    super('this record changed: reopen it')
    this.name = 'RecordChanged'
  }
}

export class NotWriter extends Refused {
  constructor() {
    super('changes are being made in another tab')
    this.name = 'NotWriter'
  }
}

export class CopyChanged extends Refused {
  constructor() {
    super('the downloaded copy changed meanwhile: review it again')
    this.name = 'CopyChanged'
  }
}

export class StaleDownload extends Refused {
  constructor() {
    super('a newer download is held')
    this.name = 'StaleDownload'
  }
}

export class AccountLapsed extends Refused {
  constructor() {
    super('the account signed in here is not this one')
    this.name = 'AccountLapsed'
  }
}

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

export const mayHaveBeenSent = (item: Pick<OutboxItem, 'sends' | 'lease' | 'unconfirmed' | 'attempts'>): boolean =>
  (item.sends ?? 0) > 0 || (item.attempts ?? 0) > 0 || item.lease !== undefined || item.unconfirmed === true

export const queueSignature = (items: readonly OutboxItem[]): string =>
  items
    .filter((i) => kindOf(i) !== 'insert')
    .map((i) => `${i.id}:${versionOf(i)}:${i.state}:${i.unconfirmed ? 1 : 0}`)
    .sort()
    .join(',')

export const WRITER = 'writer'
export type WriterRow = { key: typeof WRITER; owner: string; until: number; via?: 'lock' | 'lease' }

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

export type TabChannel = Pick<BroadcastChannel, 'postMessage' | 'close' | 'onmessage'>

const staleView = (e: unknown) =>
  e instanceof ItemChanged || e instanceof NotWriter || e instanceof RecordChanged || e instanceof CopyChanged

export class OfflineStore {
  private listeners = new Set<Listener>()
  private closed = false
  private tabs: TabChannel | null = null
  private readonly tabId = Math.random().toString(36).slice(2)
  private writer: string | undefined
  private clock: () => number = Date.now
  private account: () => boolean = () => true

  private constructor(
    readonly user: number,
    private db: IDBDatabase,
  ) {}

  static databaseName(user: number): string {
    return `taskto-offline-${user}`
  }

  static open(user: number): Promise<OfflineStore> {
    return withLock(user, () => OfflineStore.openNow(user))
  }

  private static async openNow(user: number): Promise<OfflineStore> {
    const name = OfflineStore.databaseName(user)
    const db = await openDatabase(name, SCHEMA_VERSION, MIGRATIONS)
    let retired = false
    try {
      retired = (await runTx(db, [META], 'readonly', (tx) => tx.store(META).get(RETIRED))) !== undefined
    } catch {}
    if (!retired) return new OfflineStore(user, db)
    db.close()
    await deleteDatabase(name)
    return new OfflineStore(user, await openDatabase(name, SCHEMA_VERSION, MIGRATIONS))
  }

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
    } catch {}
  }

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
    } catch {}
    this.tabs = null
  }

  refresh() {
    if (!this.closed) this.notify()
  }

  attachWriter(owner: string | undefined, now: () => number = Date.now) {
    this.writer = owner
    this.clock = now
  }

  requireAccount(check: () => boolean) {
    this.account = check
  }

  private assertAccount() {
    if (!this.account()) throw new AccountLapsed()
  }

  private async assertWriter(tx: Tx) {
    this.assertAccount()
    if (this.writer === undefined) return
    const row = await tx.store<WriterRow>(META).get(WRITER)
    if (row?.owner !== this.writer || row.until <= this.clock()) throw new NotWriter()
  }

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

  async releaseWriter(owner: string): Promise<void> {
    await this.tx([META], 'readwrite', async (tx) => {
      const meta = tx.store<WriterRow>(META)
      if ((await meta.get(WRITER))?.owner === owner) await meta.delete(WRITER)
    })
  }

  async writerIs(owner: string): Promise<boolean> {
    return (await this.tx([META], 'readonly', (tx) => tx.store<WriterRow>(META).get(WRITER)))?.owner === owner
  }

  private tx<R>(stores: string[], mode: IDBTransactionMode, fn: (tx: Tx) => Promise<R>): Promise<R> {
    if (this.closed) return Promise.reject(new Error('the offline database is closed'))
    return runTx(this.db, stores, mode, fn)
  }

  private async keep<R>(stores: string[], fn: (tx: Tx) => Promise<R>): Promise<R> {
    try {
      const out = await this.tx(stores, 'readwrite', async (tx) => {
        this.assertAccount()
        const result = await fn(tx)
        this.assertAccount()
        return result
      })
      this.changed()
      return out
    } catch (e) {
      if (e instanceof Refused) {
        if (staleView(e)) this.refresh()
        throw e
      }
      if (e instanceof Superseded) throw e
      throw asNotSaved(e)
    }
  }

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

  beginDownload(): Promise<number> {
    return this.keep([META], async (tx) => nextCounter(tx, 'downloadTicket'))
  }

  async downloadGeneration(scope: Scope): Promise<number | null> {
    return (await this.getDownload(scope))?.generation ?? null
  }

  getDownload(scope: Scope): Promise<Download | undefined> {
    return this.tx([DOWNLOADS], 'readonly', (tx) => tx.store<Download>(DOWNLOADS).get(scopeKey(scope)))
  }

  listDownloads(): Promise<Download[]> {
    return this.tx([DOWNLOADS], 'readonly', (tx) => tx.store<Download>(DOWNLOADS).getAll())
  }

  removeDownload(scope: Scope): Promise<void> {
    return this.keep([DOWNLOADS, META], async (tx) => {
      await nextGeneration(tx)
      const began = ((await tx.store<{ key: string; value: number }>(META).get('downloadTicket'))?.value ?? 0) as number
      await tx.store(META).put({ key: floorKey(scopeKey(scope)), value: began })
      await tx.store(DOWNLOADS).delete(scopeKey(scope))
    })
  }

  async enqueue(item: Omit<OutboxItem, 'seq' | 'version'>, blobs: BlobRow[]): Promise<OutboxItem> {
    return this.keep([OUTBOX, BLOBS, META], async (tx) => {
      const meta = tx.store<{ key: string; value: number }>(META)
      if ((await meta.get(RETIRED)) !== undefined) throw new NotSaved('unavailable')
      const seq = ((await meta.get('seq'))?.value ?? 0) + 1
      await meta.put({ key: 'seq', value: seq })
      const saved = { ...item, seq, version: 1 }
      await tx.store<OutboxItem>(OUTBOX).put(saved)
      for (const b of blobs) await tx.store<BlobRow>(BLOBS).put(b)
      return saved
    })
  }

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

  async listItems(scope?: Scope): Promise<OutboxItem[]> {
    const all = await this.tx([OUTBOX], 'readonly', (tx) =>
      scope
        ? tx.store<OutboxItem>(OUTBOX).getAllByIndex('byApp', scopeKey(scope))
        : tx.store<OutboxItem>(OUTBOX).getAll(),
    )
    return all.sort((a, b) => a.seq - b.seq)
  }

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

  async updateItem(
    id: string,
    change: (item: OutboxItem) => OutboxItem | undefined,
    { bump = true, guarded = false, seen }: { bump?: boolean; guarded?: boolean; seen?: SeenAs } = {},
  ): Promise<OutboxItem | undefined> {
    return this.keep(guarded ? [OUTBOX, META] : [OUTBOX], async (tx) => {
      if (guarded) await this.assertWriter(tx)
      const store = tx.store<OutboxItem>(OUTBOX)
      const current = await store.get(id)
      if (seen && (!current || !isSeenAs(current, seen))) throw new ItemChanged()
      if (!current) return undefined
      const next = change(current)
      if (!next) return current
      this.assertAccount()
      const saved = { ...next, version: versionOf(current) + (bump ? 1 : 0), updatedAt: new Date().toISOString() }
      await store.put(saved)
      return saved
    })
  }

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

  async removeItem(id: string): Promise<void> {
    await this.keep([OUTBOX, BLOBS], async (tx) => {
      const outbox = tx.store<OutboxItem>(OUTBOX)
      await outbox.delete(id)
      const blobs = tx.store<BlobRow>(BLOBS)
      for (const b of await blobs.getAll()) if (b.item === id) await blobs.delete(b.id)
    })
  }

  dropBlobs(item: string): Promise<void> {
    return this.keep([BLOBS], async (tx) => {
      const blobs = tx.store<BlobRow>(BLOBS)
      for (const b of await blobs.getAll()) if (b.item === item) await blobs.delete(b.id)
    })
  }

  async unsynced(): Promise<OutboxItem[]> {
    return (await this.listItems()).filter((i) => i.state !== 'synced')
  }

  async unapproved(approved: Approval): Promise<UnsentRef[]> {
    return (await this.unsynced())
      .filter((i) => {
        const shown = approved.get(i.id)
        return shown === undefined || !isSeen(i, shown)
      })
      .map((i): UnsentRef => ({ id: i.id, ...seenOf(i) }))
  }

  async retireOrKeepUnsent(
    approved: Approval = new Map(),
    seal = true,
    still: () => boolean = () => true,
  ): Promise<UnsentRef[]> {
    const go = () => {
      if (!still()) throw new Superseded()
    }
    return this.keep([DOWNLOADS, OUTBOX, BLOBS, META], async (tx) => {
      const outbox = tx.store<OutboxItem>(OUTBOX)
      const items = await outbox.getAll()
      go()
      const unsent = items.filter((i) => i.state !== 'synced')
      const everyApproved = unsent.every((i) => {
        const shown = approved.get(i.id)
        return shown !== undefined && isSeen(i, shown)
      })
      const refs = unsent.map((i): UnsentRef => ({ id: i.id, ...seenOf(i) }))
      if (!everyApproved && !seal) return refs
      const downloads = tx.store<Download>(DOWNLOADS)
      const kept = await downloads.getAll()
      go()
      for (const d of kept) {
        await downloads.delete(d.key)
        go()
      }
      const blobs = tx.store<BlobRow>(BLOBS)
      if (everyApproved) {
        for (const i of items) {
          await outbox.delete(i.id)
          go()
        }
        const photos = await blobs.getAll()
        go()
        for (const b of photos) {
          await blobs.delete(b.id)
          go()
        }
        await tx.store<{ key: string; value: number }>(META).put({ key: RETIRED, value: 1 })
        go()
        return []
      }
      const gone = new Set<string>()
      for (const i of items) {
        if (i.state !== 'synced') continue
        gone.add(i.id)
        await outbox.delete(i.id)
        go()
      }
      const photos = await blobs.getAll()
      go()
      for (const b of photos) {
        if (!gone.has(b.item)) continue
        await blobs.delete(b.id)
        go()
      }
      return refs
    })
  }

  close() {
    this.closed = true
    this.disconnectTabs()
    this.db.close()
  }

  async destroy(): Promise<void> {
    this.close()
    await deleteDatabase(OfflineStore.databaseName(this.user))
    this.changed()
  }

  static destroyFor(user: number): Promise<void> {
    return deleteDatabase(OfflineStore.databaseName(user))
  }

  static deleteRetired(
    user: number,
    { force = false, still = () => true }: { force?: boolean; still?: () => boolean } = {},
  ): Promise<'removed' | 'newer' | 'stopped'> {
    return withLock(user, async () => {
      if (!still()) return 'stopped'
      if (typeof indexedDB === 'undefined') return 'removed'
      const name = OfflineStore.databaseName(user)
      if (!force && !(await retiredOrNew(name))) return 'newer'
      if (!still()) return 'stopped'
      await deleteDatabase(name)
      return 'removed'
    })
  }
}

export function databaseExists(user: number): Promise<boolean | undefined> {
  if (typeof indexedDB === 'undefined') return Promise.resolve(false)
  const name = OfflineStore.databaseName(user)
  return (async () => {
    const names = await indexedDB.databases?.().catch(() => undefined)
    if (names) return names.some((d) => d.name === name)
    return withLock(user, () => probe(name))
  })()
}

function probe(name: string): Promise<boolean | undefined> {
  return new Promise((resolve) => {
    let made = false
    let request: IDBOpenDBRequest
    try {
      request = indexedDB.open(name)
    } catch {
      resolve(undefined)
      return
    }
    request.onupgradeneeded = (e) => {
      if (e.oldVersion !== 0) return
      made = true
      request.transaction?.abort()
    }
    request.onsuccess = () => {
      request.result.close()
      if (!made) return resolve(true)
      deleteDatabase(name).then(
        () => resolve(false),
        () => resolve(undefined),
      )
    }
    request.onerror = (e) => {
      e.preventDefault?.()
      resolve(made ? false : undefined)
    }
    request.onblocked = () => resolve(undefined)
  })
}

export const lockTimes = {
  wait: 10_000,
  hold: 10_000,
  waitOver: (ms: number): AbortSignal | undefined =>
    typeof AbortSignal.timeout === 'function' ? AbortSignal.timeout(ms) : undefined,
  holdOver: (ms: number, over: () => void): (() => void) => {
    const timer = setTimeout(over, ms)
    return () => clearTimeout(timer)
  },
}

export class Superseded extends Error {
  constructor() {
    super('a newer login of the account started')
    this.name = 'Superseded'
  }
}

function withLock<T>(user: number, work: () => Promise<T>): Promise<T> {
  const locks = typeof navigator !== 'undefined' ? navigator.locks : undefined
  if (!locks?.request) return work()
  let granted = false
  const signal = lockTimes.waitOver(lockTimes.wait)
  const held = () => {
    granted = true
    return withinTime(work(), lockTimes.hold)
  }
  return (locks.request(`taskto-offline-db:${user}`, signal ? { signal } : {}, held) as Promise<T>).catch((e) => {
    if (!granted && signal?.aborted) throw new StorageUnavailable('the offline database is busy in another tab')
    throw e
  })
}

function withinTime<T>(work: Promise<T>, ms: number): Promise<T> {
  return new Promise((resolve, reject) => {
    const cancel = lockTimes.holdOver(ms, () => reject(new StorageUnavailable('the offline database took too long')))
    work.then(
      (v) => {
        cancel()
        resolve(v)
      },
      (e) => {
        cancel()
        reject(e)
      },
    )
  })
}

async function retiredOrNew(name: string): Promise<boolean> {
  let made = false
  const db = await openDatabase(name, SCHEMA_VERSION, {
    ...MIGRATIONS,
    1: (d, tx) => {
      made = true
      MIGRATIONS[1]?.(d, tx)
    },
  })
  try {
    if (made) return true
    return (await runTx(db, [META], 'readonly', (tx) => tx.store(META).get(RETIRED))) !== undefined
  } catch {
    return false
  } finally {
    db.close()
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

async function nextGeneration(tx: Tx): Promise<number> {
  return nextCounter(tx, 'downloadGen')
}

async function nextCounter(tx: Tx, key: string): Promise<number> {
  const meta = tx.store<{ key: string; value: number }>(META)
  const next = ((await meta.get(key))?.value ?? 0) + 1
  await meta.put({ key, value: next })
  return next
}

const floorKey = (key: string) => `downloadFloor:${key}`
