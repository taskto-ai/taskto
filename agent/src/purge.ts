// The SQLite work of a purge: deleting a retired conversation's records from the sidecar's database,
// and emptying the write-ahead log after it.
//
// Pi Durable has no way to delete a conversation, so this is a second connection to its database
// (WAL: it waits for the harness's writes, and the harness for its). node:sqlite is synchronous, and
// a purge can wait up to its busy timeout for the write lock or for a reader that pins the log; on
// the HTTP server's thread that wait would stall every other conversation's turn and /healthz. So
// the sidecar runs it on a worker thread of its own (runPurge), one purge at a time (purgeOnce), and
// the main thread only awaits it.
//
// The harness writes on the main thread, also synchronously, and waits there whenever another
// connection holds SQLite's write lock. So the purge never holds it for long: it deletes in short
// transactions, each a bounded amount of work (CHUNK records a step, SLICE_STEPS steps and
// SLICE_PAGES freed pages a transaction, counted, not timed), with a pause between them for the
// harness's writes, in which what the transaction wrote is copied into the database (so the log
// never piles up for one long copy at the end); and it empties the log by tries that don't wait (a
// checkpoint that waits for readers holds the write lock all the while), sleeping between them on
// its own thread. And the harness's commits don't checkpoint the log either (HARNESS_DB_OPTIONS):
// one would copy, on the HTTP server's thread, what a purge or a reader let pile up. A thread of its
// own does (startCheckpointer).
//
// This module is also the worker's entry: it imports nothing of the sidecar's, so the same file
// runs as the worker in the built sidecar (dist/purge.js) and in the tests (src/purge.ts).

import { DatabaseSync } from 'node:sqlite'
import { isMainThread, parentPort, Worker, workerData } from 'node:worker_threads'

/** The log couldn't be truncated: a reader still holds it. The purge isn't done, and is asked again. */
export class WalBusyError extends Error {}

/** Folds the write-ahead log into the database file and empties it, or throws WalBusyError. */
export function truncateWal(db: DatabaseSync): void {
  // SQLite reports a checkpoint that a reader held up as a result row (busy = 1) rather than an
  // error, so the result is read.
  let busy: number
  try {
    const row = db.prepare('PRAGMA wal_checkpoint(TRUNCATE)').get() as { busy: number | bigint } | undefined
    busy = Number(row?.busy ?? 1)
  } catch (error) {
    throw new WalBusyError(`the write-ahead log couldn't be checkpointed: ${(error as Error).message}`)
  }
  if (busy !== 0) throw new WalBusyError('a reader holds the write-ahead log, so it could not be truncated')
}

/** Blocks the calling thread for ms: the purge's worker, never the sidecar's own thread. */
function sleep(ms: number): void {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms)
}

/** How long the purge leaves the write lock free between two of its transactions, and between tries. */
const PAUSE_MS = 30

/**
 * How many frames of the log may still be waiting to be copied when the purge empties it: the
 * TRUNCATE checkpoint copies them under the write lock, so it is only tried on a log that a passive
 * checkpoint has just found (all but) drained.
 */
const DRAINED_FRAMES = 64

/** What a checkpoint's row says, as SQLite returns it. */
type CheckpointRow = { busy: number | bigint; log: number | bigint; checkpointed: number | bigint }

/**
 * How many frames of the log a checkpoint's row says are left to copy: Infinity when it couldn't
 * look (busy: another connection's checkpoint holds the checkpoint lock, which SQLite reports as
 * busy = 1 with log and checkpointed -1, not as an error), and 0 only when SQLite says the database
 * isn't in WAL mode (busy = 0 with log -1): nothing to copy.
 */
export function framesLeft(row: CheckpointRow): number {
  if (Number(row.busy) !== 0) return Number.POSITIVE_INFINITY
  const log = Number(row.log)
  if (log < 0) return 0
  return log - Number(row.checkpointed)
}

/**
 * Copies what it can of the log into the database by a checkpoint that never waits and never holds
 * the harness's writes up (PASSIVE), and says how many frames are left to copy (framesLeft): some
 * while a reader's snapshot still needs them, all when another checkpoint was at it. Throws what
 * SQLite throws.
 */
function passive(db: DatabaseSync): number {
  const row = db.prepare('PRAGMA wal_checkpoint(PASSIVE)').get() as CheckpointRow | undefined
  if (!row) throw new Error('the checkpoint returned no row')
  return framesLeft(row)
}

/** passive, with Infinity when it couldn't run (a writer or another checkpoint was at it). */
function backlog(db: DatabaseSync): number {
  try {
    return passive(db)
  } catch {
    return Number.POSITIVE_INFINITY
  }
}

/**
 * How long the passive checkpoint that finds the log drained may have taken for the TRUNCATE to
 * follow it: SQLite reads how far the log goes when that checkpoint starts, so what is committed
 * while it copies is left for the TRUNCATE, under the lock. A long one (a large backlog) is followed
 * by another round, whose checkpoint copies that, quickly.
 */
const QUICK_MS = 20

/** What truncateWalWithin tells a test. */
export type TruncateHooks = {
  /** Each round's backlog after its passive checkpoint, and how long that took, before it decides. */
  round?: (backlog: number, ms: number) => void
  /** Just before the TRUNCATE checkpoint, the backlog its round found. */
  truncating?: (backlog: number) => void
  /** How long the TRUNCATE checkpoint that emptied the log took, holding the write lock, in ms. */
  truncated?: (ms: number) => void
}

/**
 * truncateWal, tried until timeoutMs has passed, never holding the write lock while it waits for a
 * reader or copies a backlog: each round copies the log by a passive checkpoint (which writers go
 * on beside, and which waits for no one), and when that found it drained (DRAINED_FRAMES at most)
 * tries the TRUNCATE at once, which holds the lock while it runs (busy_timeout 0: it doesn't wait for
 * readers or a writer either, and a round that finds one is tried again after a pause, a sleep of
 * this thread). A round whose passive checkpoint found another checkpoint at work learned nothing,
 * and is tried again too, and so is one whose checkpoint took longer than QUICK_MS. A reader that
 * lets go of its snapshot lets the next round copy the rest. Throws WalBusyError if a reader still
 * holds the log at the end.
 *
 * The bound: under the lock, the TRUNCATE copies what the passive checkpoint left (DRAINED_FRAMES at
 * most) and whatever was committed while that checkpoint ran (QUICK_MS at most) and between the two
 * calls, which nothing gates: normally nothing or the commit that was being written, but as many
 * commits as finish in that moment if this thread is held up (each a turn's write, which the sidecar
 * keeps small: a request's body is at most 1 MiB). A writer that holds the lock when the TRUNCATE
 * starts makes it busy instead.
 */
export function truncateWalWithin(db: DatabaseSync, timeoutMs: number, hooks?: TruncateHooks): void {
  db.exec('PRAGMA busy_timeout = 0')
  const deadline = performance.now() + timeoutMs
  let last: unknown
  for (;;) {
    const began = performance.now()
    const left = backlog(db)
    const took = performance.now() - began
    hooks?.round?.(left, took)
    // Looked at once more just before the TRUNCATE (a second passive checkpoint, which copies what
    // landed since the first): what the TRUNCATE copies is then only what lands after that look.
    if (left <= DRAINED_FRAMES && took <= QUICK_MS && backlog(db) <= DRAINED_FRAMES) {
      hooks?.truncating?.(left)
      try {
        const t0 = performance.now()
        truncateWal(db)
        hooks?.truncated?.(performance.now() - t0)
        return
      } catch (error) {
        if (!(error instanceof WalBusyError)) throw error
        last = error
      }
    }
    if (performance.now() + PAUSE_MS > deadline) {
      throw last instanceof WalBusyError
        ? last
        : new WalBusyError('a reader holds the write-ahead log, so it could not be copied and truncated')
    }
    sleep(PAUSE_MS)
  }
}

/** Empties the log of the database at path, on a connection of its own, trying for up to timeoutMs. */
export function truncateWalAt(path: string, timeoutMs: number): void {
  const db = new DatabaseSync(path, { timeout: timeoutMs })
  try {
    truncateWalWithin(db, timeoutMs)
  } finally {
    db.close()
  }
}

/**
 * The kind of the sidecar's document that says when a conversation was created and retired
 * (index.ts, Lifecycle). The purge removes the root's last, with the root's row.
 */
export const LIFECYCLE_KIND = 'taskto.lifecycle'

/**
 * A document's kind as Pi Durable stores it in documents.kind: as JSON (its storage writes every
 * indexed string column with JSON.stringify, encodeIndexedString in storage/sqlite/rows.js, which the
 * package doesn't export), so the lifecycle's is '"taskto.lifecycle"'. A test checks it against a
 * document Pi Durable wrote.
 */
export function storedKind(kind: string): string {
  return JSON.stringify(kind)
}

/** The lifecycle's stored kind as an SQL string literal. */
const LIFECYCLE = `'${storedKind(LIFECYCLE_KIND).replaceAll("'", "''")}'`

/**
 * How many records one step of the purge takes at most: one statement's worth, so a step's own
 * length, which the transaction's time limit (SLICE_MS, checked between steps) can't cut, stays
 * short (measured with a lock probe on 60,000 tasks of 4 KB: steps of 200 held the lock up to
 * 480 ms, of 50 about 130 ms on a busy disk, about 30 ms on a quiet one).
 */
export const CHUNK = 50

/** How many marked conversations and tasks one step of the sweep takes, and marks at most. */
const SWEEP = CHUNK

/** How many steps one of the purge's transactions takes at most, whatever the time. */
export const SLICE_STEPS = 64

/** How long one of the purge's transactions goes on deleting before it commits and pauses. */
const SLICE_MS = 20

/**
 * How many pages one of the purge's transactions frees at most: each is written at the commit
 * (overwritten with zeros, secure_delete), so this bounds the commit's work as SLICE_STEPS and
 * CHUNK bound the rows. A step takes only as many rows as fit in what is left of it, by the most
 * pages a row of that table has freed so far in the purge (PageBudget).
 */
export const SLICE_PAGES = 512

/**
 * One short transaction's time, in ms (taking the write lock, the deleting, and the commit, and its
 * longest step), and its work: the pages it freed, its steps, the most records one step took, and
 * the most rows one step changed (records with their record_ids rows and the sweep's marks).
 */
export type SliceTiming = {
  lock: number
  work: number
  commit: number
  pages: number
  step: number
  steps: number
  records: number
  changes: number
}

/** The work of the purge's last transaction: the rows it changed and the pages it freed. */
export type FinalWork = { changes: number; pages: number }

/** What purgeRecords tells a test. */
export type PurgeHooks = {
  /** Runs after each short transaction commits. */
  afterSlice?: () => void
  /** Each short transaction's length, from taking the write lock to its commit, in ms, and its parts. */
  slice?: (ms: number, timing: SliceTiming) => void
  /** The last transaction's length, from taking the write lock to its commit, in ms, and its work. */
  final?: (ms: number, work: FinalWork) => void
  /** The TRUNCATE checkpoint's length at the end, holding the write lock, in ms. */
  truncate?: (ms: number) => void
}

/** What a step returns when the transaction has no room left for a row: commit, and go on after. */
const NO_ROOM = -1

/**
 * The pages one transaction may still free, and how many rows of a table the next delete may take
 * so that it stays within them: as many as fit by the most pages a row of that table has freed
 * so far in the purge, at most CHUNK and at most twice as many as before (the first delete of a
 * table takes one row, and only first in a transaction, to learn its size). The first delete of a
 * transaction takes one row at least, so a row larger than SLICE_PAGES goes alone; otherwise a
 * transaction goes past SLICE_PAGES only by a delete whose rows are larger than any of their table
 * before them. The pages are counted by delete (what the freelist gained), so pages the sweep's
 * marks take back from the freelist don't make room.
 */
class PageBudget {
  private spent = 0
  private deleted = false
  private readonly seen = new Map<string, { perRow: number; rows: number }>()
  private readonly freePages: () => number
  constructor(freePages: () => number) {
    this.freePages = freePages
  }

  /** A transaction begins. */
  begin(): void {
    this.spent = 0
    this.deleted = false
  }

  /** The pages the transaction's deletes have freed. */
  freed(): number {
    return this.spent
  }

  /** How many rows of table the next delete may take: 0 when the transaction has no room for one. */
  private rows(table: string): number {
    const seen = this.seen.get(table)
    if (!this.deleted) return seen ? Math.max(1, this.fit(seen)) : 1
    return seen ? this.fit(seen) : 0
  }

  private fit(seen: { perRow: number; rows: number }): number {
    const ramp = Math.min(CHUNK, seen.rows * 2)
    if (seen.perRow === 0) return ramp
    return Math.max(0, Math.min(ramp, Math.floor((SLICE_PAGES - this.spent) / seen.perRow)))
  }

  /**
   * Deletes rows of table by remove(limit), which takes up to limit and says how many it took, and
   * learns their size; NO_ROOM, with nothing deleted, when the transaction has no room for one of
   * them and there is one (any(), asked only then).
   */
  take(table: string, remove: (limit: number) => number, any: () => boolean): number {
    const limit = this.rows(table)
    if (limit === 0) return any() ? NO_ROOM : 0
    const before = this.freePages()
    const n = remove(limit)
    if (n > 0) {
      const pages = Math.max(0, this.freePages() - before)
      this.spent += pages
      this.deleted = true
      const seen = this.seen.get(table) ?? { perRow: 0, rows: 0 }
      seen.perRow = Math.max(seen.perRow, Math.ceil(pages / n))
      seen.rows = Math.max(seen.rows, n)
      this.seen.set(table, seen)
    }
    return n
  }
}

// Deletes a conversation's records, and those of the conversations its tasks started, from the
// database at path: its entries (what was said), tasks, submissions and documents with their
// revisions, each record with its row of record_ids. Only a retired conversation is purged:
// retiring it stopped all its work, background tasks included, so nothing writes the rows that go.
// secure_delete zeroes the deleted content in the file, and the checkpoint takes it out of the
// write-ahead log.
//
// Everything goes in short transactions (see the top of the file), each doing a bounded amount of
// work whatever the tree's size, in an order that lets a purge that stops halfway be finished by the
// next, which carries on from what this one left in taskto_purge_marks without looking at the rest
// again. The purge marks the root, and then sweeps the marks (sweepStep): what a marked conversation
// or task started is marked before it goes, a batch at a time; then what is left of its records
// goes, a chunk at a time, and its row (its mark stays, done); a second pass looks for anything that
// refers to what is done (a late record) before it forgets the marks. A document's revisions go
// with it. The root's row, its lifecycle document, which says it is retired (so the purge goes on,
// and it stays closed to new input), and its mark go last, in a transaction that finds what is left
// from the root and the marks left: normally the root alone.
export function purgeRecords(path: string, root: number, timeoutMs: number, hooks?: PurgeHooks): void {
  const db = new DatabaseSync(path, { timeout: timeoutMs })
  try {
    db.exec('PRAGMA secure_delete = ON')
    // As the harness's own connection: a commit doesn't wait for the disk, which would hold the
    // write lock for its sync. The checkpoint that ends the purge syncs the database, and the purge
    // isn't done (the server asks again) until it has.
    db.exec('PRAGMA synchronous = NORMAL')
    // And, as the harness's, a commit doesn't checkpoint the log: the checkpointer does, on its own
    // thread, so no commit of the purge's does that work besides its own.
    db.exec('PRAGMA wal_autocheckpoint = 0')
    // Pi Durable never gives an id again (its next_id only grows), so a mark names nothing else.
    db.exec(MARKS_TABLE)
    db.exec(MARKS_INDEX)
    db.prepare(`INSERT OR IGNORE INTO taskto_purge_marks (root, kind, id) VALUES (?, 'conversation', ?)`).run(
      root,
      root,
    )
    const freelist = db.prepare('PRAGMA freelist_count')
    const freePages = () => Number((freelist.get() as { freelist_count: number | bigint }).freelist_count)
    const totalChanges = db.prepare('SELECT total_changes() AS n')
    const changes = () => Number((totalChanges.get() as { n: number | bigint }).n)
    const budget = new PageBudget(freePages)
    const step = sweepStep(db, root, budget)
    // Transactions of at most SLICE_STEPS steps of CHUNK records each, SLICE_PAGES freed and about
    // SLICE_MS, with a pause between them in which the harness's writes get the lock.
    for (let more = true; more; ) {
      const began = performance.now()
      db.exec('BEGIN IMMEDIATE')
      const locked = performance.now()
      let worked = locked
      let freed = 0
      let longestStep = 0
      let steps = 0
      let records = 0
      let mostChanged = 0
      try {
        budget.begin()
        while (steps < SLICE_STEPS && performance.now() - locked < SLICE_MS) {
          const t0 = performance.now()
          const c0 = changes()
          const n = step()
          longestStep = Math.max(longestStep, performance.now() - t0)
          if (n === NO_ROOM) break
          steps++
          records = Math.max(records, n)
          mostChanged = Math.max(mostChanged, changes() - c0)
          if (n === 0) {
            more = false
            break
          }
        }
        freed = budget.freed()
        worked = performance.now()
        db.exec('COMMIT')
      } catch (error) {
        db.exec('ROLLBACK')
        throw error
      }
      const ended = performance.now()
      hooks?.slice?.(ended - began, {
        lock: locked - began,
        work: worked - locked,
        commit: ended - worked,
        pages: freed,
        step: longestStep,
        steps,
        records,
        changes: mostChanged,
      })
      hooks?.afterSlice?.()
      // What the transaction wrote is copied into the database now, by a checkpoint that waits for
      // no one and holds no lock the harness's writes need, and syncs what it copied: a log left to
      // pile up was copied at the end in one go (the whole account, 250 MB for 60,000 tasks of
      // 4 KB), and while the system wrote that out a one-row commit of the harness's took over
      // 300 ms in its own write, holding the write lock all the while (traced on every connection).
      backlog(db)
      sleep(PAUSE_MS)
    }
    const began = performance.now()
    db.exec('BEGIN IMMEDIATE')
    let finalWork: FinalWork
    try {
      const c0 = changes()
      const f0 = freePages()
      // Found again under the write lock, from the root and the marks left: what was written during
      // the pauses goes too. The sweep has taken the rest, so this is the root and what is late.
      const tree = discover(db, root, marked(db, root))
      for (const step of contentSteps(db, tree)) while (step() > 0);
      const ids = list(tree.ids)
      removeDocuments(db, select(db, `SELECT id FROM documents WHERE ${ownedBy(tree)}`))
      removeRows(db, 'tasks', select(db, `SELECT id FROM tasks WHERE conversation_id IN (${ids})`))
      removeRows(db, 'conversations', select(db, `SELECT id FROM conversations WHERE id IN (${ids})`))
      db.prepare('DELETE FROM taskto_purge_marks WHERE root = ?').run(root)
      finalWork = { changes: changes() - c0, pages: freePages() - f0 }
      db.exec('COMMIT')
    } catch (error) {
      db.exec('ROLLBACK')
      throw error
    }
    hooks?.final?.(performance.now() - began, finalWork)
    // What was deleted is still in the log until it is emptied, and a reader (the harness's, or a
    // watch) can hold it up: then the purge isn't done, and the server asks again.
    truncateWalWithin(db, timeoutMs, hooks?.truncate ? { truncated: hooks.truncate } : undefined)
  } finally {
    db.close()
  }
}

/**
 * The conversations and tasks of each purge that has begun and not ended, by its root: the
 * sidecar's own table in Pi Durable's database, which Pi Durable doesn't read. done: the row it
 * names is gone, and the mark waits for the sweep's second look. The root's marks go in the
 * transaction that ends the purge.
 */
const MARKS_TABLE = `CREATE TABLE IF NOT EXISTS taskto_purge_marks (
  root INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('conversation', 'task')),
  id   INTEGER NOT NULL,
  done INTEGER NOT NULL DEFAULT 0 CHECK (done IN (0, 1)),
  PRIMARY KEY (root, kind, id)
) STRICT`

/** The sweep's batches come from this index alone, in its order: no sort, however many marks. */
const MARKS_INDEX =
  'CREATE INDEX IF NOT EXISTS taskto_purge_marks_by_state ON taskto_purge_marks (root, done, kind, id)'

// The marks left of root's purge (after the sweep: the root's, and any made since).
function marked(db: DatabaseSync, root: number): Tree {
  const tree: Tree = { ids: new Set(), taskIds: new Set() }
  for (const row of db.prepare('SELECT kind, id FROM taskto_purge_marks WHERE root = ?').all(root) as {
    kind: string
    id: number | bigint
  }[]) {
    ;(row.kind === 'task' ? tree.taskIds : tree.ids).add(Number(row.id))
  }
  return tree
}

/** A tree of conversations: the root, the conversations its tasks started, theirs, and so on. */
type Tree = { ids: Set<number>; taskIds: Set<number> }

/** A set of ids as SQL's list, NULL when empty (which matches nothing). */
function list(set: Set<number> | number[]): string {
  return [...set].join(',') || 'NULL'
}

/** The ids query returns, as numbers. */
function select(db: DatabaseSync, query: string): number[] {
  return (db.prepare(query).all() as { id: number | bigint }[]).map((r) => Number(r.id))
}

/** Deletes rows of table by rowid, and their record_ids rows when they are records. */
function removeRows(db: DatabaseSync, table: string, ids: number[], records = true): void {
  if (ids.length === 0) return
  db.exec(`DELETE FROM ${table} WHERE rowid IN (${ids.join(',')})`)
  if (records) db.exec(`DELETE FROM record_ids WHERE id IN (${ids.join(',')})`)
}

/**
 * Deletes documents with their revisions, in the transaction that is open: a revision written since
 * the revisions' own step would otherwise be left with no document to find it by.
 */
function removeDocuments(db: DatabaseSync, ids: number[]): void {
  if (ids.length === 0) return
  db.exec(`DELETE FROM document_revisions WHERE document_id IN (${ids.join(',')})`)
  removeRows(db, 'documents', ids)
}

/** Which documents belong to tree: its conversations' and its tasks'. */
function ownedBy(tree: Tree): string {
  return `((scope_kind = 'conversation' AND owner_id IN (${list(tree.ids)}))
    OR (scope_kind = 'task' AND owner_id IN (${list(tree.taskIds)})))`
}

/**
 * A step: one bounded piece of work in the transaction that is open, saying how much (0: done;
 * NO_ROOM: the transaction has no room for it, and it is done in the next).
 */
type Step = () => number

/**
 * A step that deletes, a chunk at a time, what query (of rowids or ids) finds: CHUNK rows, or with
 * a budget as many as it leaves room for (NO_ROOM when none).
 */
function each(
  db: DatabaseSync,
  query: string,
  remove: (chunk: number[]) => void,
  budget?: { table: string; of: PageBudget },
): Step {
  const take = (limit: number) => {
    const chunk = select(db, `${query} LIMIT ${limit}`)
    remove(chunk)
    return chunk.length
  }
  const any = () => select(db, `${query} LIMIT 1`).length > 0
  return budget ? () => budget.of.take(budget.table, take, any) : () => take(CHUNK)
}

/** The steps that delete what tree's conversations and tasks said: all but the root's lifecycle. */
function contentSteps(db: DatabaseSync, tree: Tree): Step[] {
  const ids = list(tree.ids)
  const docs = `SELECT id FROM documents WHERE ${ownedBy(tree)} AND kind <> ${LIFECYCLE}`
  return [
    each(db, `SELECT rowid AS id FROM document_revisions WHERE document_id IN (${docs})`, (c) =>
      removeRows(db, 'document_revisions', c, false),
    ),
    each(db, docs, (c) => removeDocuments(db, c)),
    each(db, `SELECT id FROM submissions WHERE conversation_id IN (${ids})`, (c) => removeRows(db, 'submissions', c)),
    each(db, `SELECT id FROM entries WHERE conversation_id IN (${ids})`, (c) => removeRows(db, 'entries', c)),
  ]
}

type Mark = { r: number | bigint; kind: string; id: number | bigint }

/**
 * The sweep of root's marks, a batch of SWEEP at a time from MARKS_INDEX, tasks before
 * conversations: a conversation's tasks, marked a batch at a time, go before more of them are
 * marked, so the look for the ones not yet marked never has many to skip.
 *
 * First pass (marks not done): what the batch's conversations and tasks started and isn't marked
 * yet is marked, SWEEP at most; then what is left of their records goes, a chunk at a time; then
 * their rows, but the root's, and their marks are done. The root's mark stays: its row and
 * lifecycle go in the purge's last transaction. Second pass (once only the root is left to do):
 * anything that still refers to a done mark (a record written late) is taken, or marked if it is a
 * conversation or a task (back to the first pass), and then the marks are forgotten. Each call does
 * a bounded amount of work; 0 when there is none left.
 */
function sweepStep(db: DatabaseSync, root: number, budget: PageBudget): Step {
  const batchOf = db.prepare(
    `SELECT rowid AS r, kind, id FROM taskto_purge_marks WHERE root = ? AND done = ?
      ORDER BY kind DESC, id DESC LIMIT ${SWEEP}`,
  )
  const isRoot = (m: Mark) => m.kind === 'conversation' && Number(m.id) === root
  const lifecycle = `NOT (scope_kind = 'conversation' AND owner_id = ${root} AND kind = ${LIFECYCLE})`
  // What was started under the batch and what is left of its records, a bounded piece: how much.
  const work = (batch: Mark[]): number => {
    const c = list(batch.filter((m) => m.kind === 'conversation').map((m) => Number(m.id)))
    const t = list(batch.filter((m) => m.kind === 'task').map((m) => Number(m.id)))
    // Marked before they go, so it is still found: the ones not marked yet, by one statement a
    // step, so a step marks SWEEP at most.
    const started = db
      .prepare(`INSERT OR IGNORE INTO taskto_purge_marks (root, kind, id)
        SELECT ?, 'task', x.id FROM tasks x WHERE x.conversation_id IN (${c})
          AND NOT EXISTS (SELECT 1 FROM taskto_purge_marks m WHERE m.root = ? AND m.kind = 'task' AND m.id = x.id)
        LIMIT ${SWEEP}`)
      .run(root, root)
    if (Number(started.changes) > 0) return Number(started.changes)
    const owned = db
      .prepare(`INSERT OR IGNORE INTO taskto_purge_marks (root, kind, id)
        SELECT ?, 'conversation', x.id FROM conversations x WHERE x.owner_task_id IN (${t})
          AND NOT EXISTS (SELECT 1 FROM taskto_purge_marks m WHERE m.root = ? AND m.kind = 'conversation' AND m.id = x.id)
        LIMIT ${SWEEP}`)
      .run(root, root)
    if (Number(owned.changes) > 0) return Number(owned.changes)
    // What is left of their records, a chunk at a time within the transaction's pages (the root's
    // lifecycle document stays).
    const docs = `SELECT id FROM documents WHERE ((scope_kind = 'conversation' AND owner_id IN (${c}))
      OR (scope_kind = 'task' AND owner_id IN (${t}))) AND ${lifecycle}`
    for (const step of [
      each(
        db,
        `SELECT rowid AS id FROM document_revisions WHERE document_id IN (${docs})`,
        (x) => removeRows(db, 'document_revisions', x, false),
        { table: 'document_revisions', of: budget },
      ),
      each(db, docs, (x) => removeDocuments(db, x), { table: 'documents', of: budget }),
      each(db, `SELECT id FROM submissions WHERE conversation_id IN (${c})`, (x) => removeRows(db, 'submissions', x), {
        table: 'submissions',
        of: budget,
      }),
      each(db, `SELECT id FROM entries WHERE conversation_id IN (${c})`, (x) => removeRows(db, 'entries', x), {
        table: 'entries',
        of: budget,
      }),
    ]) {
      const n = step()
      if (n !== 0) return n
    }
    return 0
  }
  return () => {
    const todo = batchOf.all(root, 0) as Mark[]
    if (todo.length > 0) {
      const n = work(todo)
      if (n !== 0) return n
      const others = todo.filter((m) => !isRoot(m))
      if (others.length > 0) {
        // Nothing is left under any of the batch: their rows go, as many as the pages leave room
        // for (tasks first, in the batch's order), and the rest with the next step.
        return budget.take(
          'tasks and conversations',
          (limit) => {
            const some = others.slice(0, limit)
            const t = list(some.filter((m) => m.kind === 'task').map((m) => Number(m.id)))
            const c = list(some.filter((m) => m.kind === 'conversation').map((m) => Number(m.id)))
            removeRows(db, 'tasks', select(db, `SELECT id FROM tasks WHERE id IN (${t})`))
            removeRows(db, 'conversations', select(db, `SELECT id FROM conversations WHERE id IN (${c})`))
            db.exec(`UPDATE taskto_purge_marks SET done = 1 WHERE rowid IN (${some.map((m) => Number(m.r)).join(',')})`)
            return some.length
          },
          () => true,
        )
      }
      // Only the root is left to do, and nothing under it: the second pass.
    }
    const done = batchOf.all(root, 1) as Mark[]
    if (done.length === 0) return 0
    const n = work(done)
    if (n !== 0) return n // a late record taken, or what was started under it marked: looked at again
    db.exec(`DELETE FROM taskto_purge_marks WHERE rowid IN (${done.map((m) => Number(m.r)).join(',')})`)
    return done.length
  }
}

// The tree of root's purge as the database has it now, from root and, when given, the conversations
// and tasks an earlier look found (seen): what refers to one of those goes too, even once its row is
// gone.
function discover(db: DatabaseSync, root: number, seen?: Tree): Tree {
  const ids = new Set<number>([root, ...(seen?.ids ?? [])])
  const taskIds = new Set<number>(seen?.taskIds ?? [])
  for (let grew = true; grew; ) {
    grew = false
    const tasks = db.prepare(`SELECT id FROM tasks WHERE conversation_id IN (${list(ids)})`).all() as {
      id: number
    }[]
    for (const t of tasks) taskIds.add(Number(t.id))
    const subs = db.prepare(`SELECT id FROM conversations WHERE owner_task_id IN (${list(taskIds)})`).all() as {
      id: number
    }[]
    for (const c of subs) {
      if (!ids.has(Number(c.id))) {
        ids.add(Number(c.id))
        grew = true
      }
    }
  }
  return { ids, taskIds }
}

/** What runPurge does: purge a conversation's records (root), or only truncate the log (no root). */
export type PurgeJob = {
  path: string
  timeoutMs: number
  root?: number
  /** For tests: report the short transactions' times (runPurge's result). */
  report?: boolean
}

/** The short transactions of a purge, for a test: how many, and the longest of each part. */
export type PurgeReport = {
  slices: number
  longest: SliceTiming & { ms: number }
  lock: number
  work: number
  commit: number
  step: number
  /** The most pages one transaction freed, records one step took, and rows one step changed. */
  pages: number
  records: number
  changes: number
  /** The last transaction's length, and the closing TRUNCATE's, each holding the write lock, in ms. */
  final?: number
  truncate?: number
}

type Outcome = { ok: true; report?: PurgeReport } | { ok: false; busy: boolean; message: string }

const MARK = 'taskto.purge'

function run(job: PurgeJob): Outcome {
  try {
    if (job.root === undefined) {
      truncateWalAt(job.path, job.timeoutMs)
      return { ok: true }
    }
    let report: PurgeReport | undefined
    const slice = (ms: number, t: SliceTiming) => {
      report ??= {
        slices: 0,
        longest: { ms, ...t },
        lock: 0,
        work: 0,
        commit: 0,
        step: 0,
        pages: 0,
        records: 0,
        changes: 0,
      }
      report.slices++
      if (ms > report.longest.ms) report.longest = { ms, ...t }
      report.lock = Math.max(report.lock, t.lock)
      report.work = Math.max(report.work, t.work)
      report.commit = Math.max(report.commit, t.commit)
      report.step = Math.max(report.step, t.step)
      report.pages = Math.max(report.pages, t.pages)
      report.records = Math.max(report.records, t.records)
      report.changes = Math.max(report.changes, t.changes)
    }
    let final = 0
    let truncate = 0
    purgeRecords(
      job.path,
      job.root,
      job.timeoutMs,
      job.report ? { slice, final: (ms) => (final = ms), truncate: (ms) => (truncate = ms) } : undefined,
    )
    if (report) Object.assign(report, { final, truncate })
    return { ok: true, report }
  } catch (error) {
    return { ok: false, busy: error instanceof WalBusyError, message: (error as Error).message ?? String(error) }
  }
}

let workers = 0

/** How many purge workers this process has started: for tests. */
export function purgeWorkersStarted(): number {
  return workers
}

/**
 * Runs job on a worker thread of its own and settles when it is done: rejects with WalBusyError when
 * a reader held the log, and with an Error for any other failure. The calling thread is free
 * meanwhile. The sidecar calls purgeOnce, which runs one at a time.
 */
export function runPurge(job: PurgeJob): Promise<PurgeReport | undefined> {
  workers++
  running++
  mostAtOnce = Math.max(mostAtOnce, running)
  return new Promise((resolve, reject) => {
    let settled = false
    const settle = (end: () => void) => {
      if (settled) return
      settled = true
      running--
      end()
    }
    const worker = new Worker(new URL(import.meta.url), { workerData: { [MARK]: job } })
    worker.once('message', (out: Outcome) => {
      settle(() =>
        out.ok ? resolve(out.report) : reject(out.busy ? new WalBusyError(out.message) : new Error(out.message)),
      )
    })
    worker.once('error', (error) => settle(() => reject(error)))
    worker.once('exit', (code) => settle(() => reject(new Error(`the purge's worker thread exited with code ${code}`))))
  })
}

let running = 0
let mostAtOnce = 0

/** The most purge workers that have run at the same time since the last call: for tests. */
export function purgeWorkersAtOnce(): number {
  const most = mostAtOnce
  mostAtOnce = running
  return most
}

// The purges asked for and not ended, by what they do; and the end of the last one queued.
const asked = new Map<string, Promise<void>>()
let queue: Promise<unknown> = Promise.resolve()

/**
 * runPurge, one at a time in this process, and once for a root (or for the log alone) at a time: a
 * request for one that is queued or running waits for it and has its outcome, and starts no worker
 * of its own. A worker isn't stopped by its caller going away (the server's call has a time limit,
 * and it asks again later), so without this a slow purge and the server's next try of it would
 * take turns on the write lock and leave the harness's writes none of the pauses.
 */
export function purgeOnce(job: PurgeJob): Promise<void> {
  const key = `${job.path}\u0000${job.root ?? 'log'}`
  const running = asked.get(key)
  if (running) return running
  const result = queue.then(() => runPurge(job)).then(() => undefined)
  queue = result.catch(() => undefined)
  asked.set(key, result)
  const forget = () => {
    if (asked.get(key) === result) asked.delete(key)
  }
  result.then(forget, forget)
  return result
}

/**
 * How the sidecar opens the harness's database (openNodeSqliteStorage's options): a commit never
 * checkpoints the log, which would copy it on the HTTP server's thread, however large a purge or a
 * reader let it grow. startCheckpointer copies it instead, on a thread of its own.
 */
export const HARNESS_DB_OPTIONS = { walAutoCheckpointPages: 0 } as const

/** How often the checkpointer copies the log into the database. */
const CHECKPOINT_EVERY_MS = 1000

/** How many checkpoints in a row may fail before the checkpointer's thread is started again. */
const CHECKPOINT_FAILURES = 10

const CHECKPOINTER = 'taskto.checkpointer'

type CheckpointerJob = { path: string; everyMs: number; stop: Int32Array }

/** What startCheckpointer tells a test. */
export type CheckpointerHooks = {
  /** Each time a thread of the checkpointer starts, the first time included. */
  started?: () => void
  /** Each failed checkpoint, with how many have failed in that thread so far. */
  failed?: (count: number) => void
  /** When a thread has ended and the next is due to start, before it does. */
  restarting?: () => void
}

/**
 * Copies the log of the database at path into the database by a passive checkpoint every everyMs,
 * on a worker thread of its own (writers go on beside it, and it never waits for readers), until the
 * function it returns is called. It stands in for the commits' own checkpoints, which
 * HARNESS_DB_OPTIONS turns off. A checkpoint that fails is logged and counted; a thread whose
 * checkpoints keep failing, or that fails, is started again, unless it was stopped.
 */
export function startCheckpointer(
  path: string,
  everyMs = CHECKPOINT_EVERY_MS,
  hooks?: CheckpointerHooks,
): () => Promise<void> {
  const stop = new Int32Array(new SharedArrayBuffer(4))
  let worker: Worker | undefined
  let again: ReturnType<typeof setTimeout> | undefined
  const stopped = () => Atomics.load(stop, 0) !== 0
  const start = () => {
    again = undefined
    if (stopped()) return
    const w = new Worker(new URL(import.meta.url), { workerData: { [CHECKPOINTER]: { path, everyMs, stop } } })
    worker = w
    hooks?.started?.()
    w.on('message', (m: { failed?: number }) => {
      if (typeof m?.failed === 'number') hooks?.failed?.(m.failed)
    })
    w.on('error', (error) => console.error('the checkpointer failed; it starts again', error))
    w.once('exit', () => {
      if (!stopped()) {
        again = setTimeout(start, everyMs)
        again.unref()
        hooks?.restarting?.()
      }
    })
    // Last: a listener added after unref() refs the worker again, and then it keeps the process up.
    w.unref()
  }
  start()
  return async () => {
    Atomics.store(stop, 0, 1)
    Atomics.notify(stop, 0)
    if (again !== undefined) clearTimeout(again)
    const w = worker
    if (!w || w.threadId === -1) return // none running: nothing to wait for
    await new Promise<void>((resolve) => {
      w.once('exit', () => resolve())
      if (w.threadId === -1) resolve()
    })
  }
}

function checkpoint(job: CheckpointerJob): void {
  const db = new DatabaseSync(job.path, { timeout: 0 })
  let failures = 0
  let failed = 0
  try {
    while (Atomics.load(job.stop, 0) === 0) {
      try {
        // Another checkpoint at work (a purge's) is neither a failure nor a success: looked at next time.
        if (passive(db) !== Number.POSITIVE_INFINITY) failures = 0
      } catch (error) {
        failures++
        failed++
        console.error(`checkpoint failed (${failed} so far, ${failures} in a row): ${(error as Error).message}`)
        parentPort?.postMessage({ failed })
        if (failures >= CHECKPOINT_FAILURES) throw new Error(`${failures} checkpoints in a row failed`)
      }
      Atomics.wait(job.stop, 0, 0, job.everyMs)
    }
  } finally {
    db.close()
  }
}

// The worker's side: only in a thread runPurge or startCheckpointer started, never on the sidecar's
// own thread or in a thread someone else (a test runner) started.
const data = isMainThread ? null : (workerData as Record<string, unknown> | null)
const mine = data?.[MARK]
if (mine && parentPort) parentPort.postMessage(run(mine as PurgeJob))
const checkpointing = data?.[CHECKPOINTER]
if (checkpointing) checkpoint(checkpointing as CheckpointerJob)
