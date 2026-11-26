// The SQLite work of a purge: deleting a retired conversation's records from the sidecar's database,
// and emptying the write-ahead log after it.
//
// Pi Durable has no way to delete a conversation, so this is a second connection to its database
// (WAL: it waits for the harness's writes, and the harness for its). node:sqlite is synchronous, and
// a purge can wait up to its busy timeout for the write lock or for a reader that pins the log; on
// the HTTP server's thread that wait would stall every other conversation's turn and /healthz. So
// the sidecar runs it on a worker thread of its own (runPurge), and the main thread only awaits it.
//
// The harness writes on the main thread, also synchronously, and waits there whenever another
// connection holds SQLite's write lock. So the purge never holds it for long: it deletes in short
// transactions, a chunk of rows each, with a pause between them for the harness's writes, and it
// empties the log by tries that don't wait (a checkpoint that waits for readers holds the write lock
// all the while), sleeping between them on its own thread. And the harness's commits don't
// checkpoint the log either (HARNESS_DB_OPTIONS): one would copy, on the HTTP server's thread, what a
// purge or a reader let pile up. A thread of its own does (startCheckpointer).
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

/**
 * Copies what it can of the log into the database by a checkpoint that never waits and never holds
 * the harness's writes up (PASSIVE), and says how many frames are left to copy: some while a
 * reader's snapshot still needs them. Throws what SQLite throws.
 */
function passive(db: DatabaseSync): number {
  const row = db.prepare('PRAGMA wal_checkpoint(PASSIVE)').get() as
    | { log: number | bigint; checkpointed: number | bigint }
    | undefined
  if (!row) throw new Error('the checkpoint returned no row')
  const log = Number(row.log)
  return log < 0 ? 0 : log - Number(row.checkpointed) // -1: not in WAL mode, nothing to copy
}

/** passive, with Infinity when it couldn't run (a writer or another checkpoint was at it). */
function backlog(db: DatabaseSync): number {
  try {
    return passive(db)
  } catch {
    return Number.POSITIVE_INFINITY
  }
}

/** What truncateWalWithin tells a test. */
export type TruncateHooks = {
  /** Each round's backlog after its passive checkpoint, before the round decides what to do. */
  round?: (backlog: number) => void
  /** Just before the TRUNCATE checkpoint, the backlog its round found. */
  truncating?: (backlog: number) => void
}

/**
 * truncateWal, tried until timeoutMs has passed, never holding the write lock while it waits for a
 * reader or copies a backlog: each round copies the log by a passive checkpoint (which writers go
 * on beside, and which waits for no one), and when that found it drained (DRAINED_FRAMES at most)
 * tries the TRUNCATE at once, which holds the lock while it runs (busy_timeout 0: it doesn't wait for
 * readers or a writer either, and a round that finds one is tried again after a pause, a sleep of
 * this thread). A reader that lets go of its snapshot lets the next round copy the rest. Throws
 * WalBusyError if a reader still holds the log at the end.
 *
 * The bound: under the lock, the TRUNCATE copies what the passive checkpoint left (DRAINED_FRAMES at
 * most) and what was committed between the two, which is at most the commit that was being written
 * when the passive checkpoint ran (a writer that holds the lock when the TRUNCATE starts makes it
 * busy instead). Nothing gates the harness's writers for it.
 */
export function truncateWalWithin(db: DatabaseSync, timeoutMs: number, hooks?: TruncateHooks): void {
  db.exec('PRAGMA busy_timeout = 0')
  const deadline = performance.now() + timeoutMs
  let last: unknown
  for (;;) {
    const left = backlog(db)
    hooks?.round?.(left)
    if (left <= DRAINED_FRAMES) {
      hooks?.truncating?.(left)
      try {
        truncateWal(db)
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

/** How many rows one statement of the purge deletes: small, so a transaction stays near SLICE_MS. */
const CHUNK = 100

/** How many marked conversations and tasks one step of the sweep takes. */
const SWEEP = 200

/** How long one of the purge's transactions goes on deleting before it commits and pauses. */
const SLICE_MS = 20

/** What purgeRecords tells a test. */
export type PurgeHooks = {
  /** Runs after each short transaction commits. */
  afterSlice?: () => void
  /** The last transaction's length, from taking the write lock to its commit, in ms. */
  final?: (ms: number) => void
}

// Deletes a conversation's records, and those of the conversations its tasks started, from the
// database at path: its entries (what was said), tasks, submissions and documents with their
// revisions, each record with its row of record_ids. Only a retired conversation is purged:
// retiring it stopped all its work, background tasks included, so nothing writes the rows that go.
// secure_delete zeroes the deleted content in the file, and the checkpoint takes it out of the
// write-ahead log.
//
// Everything goes in short transactions (see the top of the file), in an order that lets a purge
// that stops halfway be finished by the next. First the tree's conversations and tasks are marked
// (taskto_purge_marks): a retry starts from the marks as well as from the root, so what a removed
// row led to is still found. Then what was said goes (documents with their revisions, submissions,
// entries), and then the sweep: each marked conversation or task, but the root, has what was
// started under it marked (in the same transaction, before it goes), what is left of its records
// removed, and its row removed (its mark stays, done); a second pass looks again for anything that
// refers to what is done (a late record) before it forgets the marks. A document's revisions go
// with it. The root's row, its lifecycle document, which says it is retired (so the purge goes on,
// and it stays closed to new input), and its marks go last, in a transaction that finds what is
// left from the root and the marks left: normally the root alone, so it stays short whatever the
// tree's size.
export function purgeRecords(path: string, root: number, timeoutMs: number, hooks?: PurgeHooks): void {
  const db = new DatabaseSync(path, { timeout: timeoutMs })
  try {
    db.exec('PRAGMA secure_delete = ON')
    // As the harness's own connection: a commit doesn't wait for the disk, which would hold the
    // write lock for its sync. The checkpoint that ends the purge syncs the database, and the purge
    // isn't done (the server asks again) until it has.
    db.exec('PRAGMA synchronous = NORMAL')
    // Pi Durable never gives an id again (its next_id only grows), so a mark names nothing else.
    db.exec(MARKS_TABLE)
    const seen = discover(db, root, marked(db, root))
    mark(db, root, seen)
    const steps = [...contentSteps(db, seen), sweepStep(db, root)]
    // Transactions of at most about SLICE_MS each, with a pause between them in which the harness's
    // writes get the lock.
    for (let at = 0; at < steps.length; ) {
      db.exec('BEGIN IMMEDIATE')
      try {
        const start = performance.now()
        while (at < steps.length && performance.now() - start < SLICE_MS) {
          if (steps[at]() === 0) at++
        }
        db.exec('COMMIT')
      } catch (error) {
        db.exec('ROLLBACK')
        throw error
      }
      hooks?.afterSlice?.()
      sleep(PAUSE_MS)
    }
    const began = performance.now()
    db.exec('BEGIN IMMEDIATE')
    try {
      // Found again under the write lock, from the root and the marks left: what was written during
      // the pauses goes too. The sweep has taken the rest, so this is the root and what is late.
      const tree = discover(db, root, marked(db, root))
      for (const step of contentSteps(db, tree)) while (step() > 0);
      const ids = list(tree.ids)
      removeDocuments(db, select(db, `SELECT id FROM documents WHERE ${ownedBy(tree)}`))
      removeRows(db, 'tasks', select(db, `SELECT id FROM tasks WHERE conversation_id IN (${ids})`))
      removeRows(db, 'conversations', select(db, `SELECT id FROM conversations WHERE id IN (${ids})`))
      db.prepare('DELETE FROM taskto_purge_marks WHERE root = ?').run(root)
      db.exec('COMMIT')
    } catch (error) {
      db.exec('ROLLBACK')
      throw error
    }
    hooks?.final?.(performance.now() - began)
    // What was deleted is still in the log until it is emptied, and a reader (the harness's, or a
    // watch) can hold it up: then the purge isn't done, and the server asks again.
    truncateWalWithin(db, timeoutMs)
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

/** How many ids one of mark's transactions writes. */
const MARK_CHUNK = 2000

// The tree an earlier, unfinished purge of root marked.
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

// Marks tree's conversations and tasks for root, in short transactions with pauses between them.
function mark(db: DatabaseSync, root: number, tree: Tree): void {
  const all = [
    ...[...tree.ids].map((id) => ['conversation', id] as const),
    ...[...tree.taskIds].map((id) => ['task', id] as const),
  ]
  for (let i = 0; i < all.length; i += MARK_CHUNK) {
    const chunk = all.slice(i, i + MARK_CHUNK)
    db.exec('BEGIN IMMEDIATE')
    try {
      const values = chunk.map(([kind, id]) => `(${root}, '${kind}', ${id})`).join(', ')
      db.exec(`INSERT OR IGNORE INTO taskto_purge_marks (root, kind, id) VALUES ${values}`)
      db.exec('COMMIT')
    } catch (error) {
      db.exec('ROLLBACK')
      throw error
    }
    sleep(PAUSE_MS)
  }
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

/** A step: one chunk in the transaction that is open, saying how many rows it took (0: done). */
type Step = () => number

/** A step that deletes, a chunk at a time, what query (of rowids or ids) finds. */
function each(db: DatabaseSync, query: string, remove: (chunk: number[]) => void): Step {
  return () => {
    const chunk = select(db, `${query} LIMIT ${CHUNK}`)
    remove(chunk)
    return chunk.length
  }
}

/** The steps that delete what tree's conversations and tasks said: all but the root's lifecycle. */
function contentSteps(db: DatabaseSync, tree: Tree): Step[] {
  const ids = list(tree.ids)
  const docs = `SELECT id FROM documents WHERE ${ownedBy(tree)} AND kind <> '${LIFECYCLE_KIND}'`
  return [
    each(db, `SELECT rowid AS id FROM document_revisions WHERE document_id IN (${docs})`, (c) =>
      removeRows(db, 'document_revisions', c, false),
    ),
    each(db, docs, (c) => removeDocuments(db, c)),
    each(db, `SELECT id FROM submissions WHERE conversation_id IN (${ids})`, (c) => removeRows(db, 'submissions', c)),
    each(db, `SELECT id FROM entries WHERE conversation_id IN (${ids})`, (c) => removeRows(db, 'entries', c)),
  ]
}

/**
 * The sweep of root's marks, but the root's own, in batches of SWEEP. First pass (marks not done):
 * what each started is marked (so it is still found once its parent is gone), what is left of its
 * records goes a chunk at a time, and then its row, and its mark is done. Second pass (once none is
 * left to do): anything that still refers to a done mark (a record written late) is taken, or
 * marked if it is a conversation or a task, and then the marks are forgotten.
 */
function sweepStep(db: DatabaseSync, root: number): Step {
  const mine = `root = ${root} AND NOT (kind = 'conversation' AND id = ${root})`
  return () => {
    let batch = db
      .prepare(
        `SELECT rowid AS r, kind, id FROM taskto_purge_marks WHERE ${mine} AND done = 0 ORDER BY rowid LIMIT ${SWEEP}`,
      )
      .all() as { r: number | bigint; kind: string; id: number | bigint }[]
    const removing = batch.length > 0
    if (!removing) {
      batch = db
        .prepare(`SELECT rowid AS r, kind, id FROM taskto_purge_marks WHERE ${mine} ORDER BY rowid LIMIT ${SWEEP}`)
        .all() as typeof batch
    }
    if (batch.length === 0) return 0
    const convs = list(batch.filter((m) => m.kind === 'conversation').map((m) => Number(m.id)))
    const tasks = list(batch.filter((m) => m.kind === 'task').map((m) => Number(m.id)))
    // What was started under them, late too, is marked before they go, so it is still found.
    const started = db
      .prepare(`INSERT OR IGNORE INTO taskto_purge_marks (root, kind, id)
        SELECT ?, 'task', id FROM tasks WHERE conversation_id IN (${convs})
        UNION ALL SELECT ?, 'conversation', id FROM conversations WHERE owner_task_id IN (${tasks})`)
      .run(root, root)
    if (Number(started.changes) > 0) return Number(started.changes) // the first pass takes them
    // What is left of their records, a chunk at a time.
    const docs = `SELECT id FROM documents WHERE (scope_kind = 'conversation' AND owner_id IN (${convs}))
      OR (scope_kind = 'task' AND owner_id IN (${tasks}))`
    for (const step of [
      each(db, `SELECT rowid AS id FROM document_revisions WHERE document_id IN (${docs})`, (c) =>
        removeRows(db, 'document_revisions', c, false),
      ),
      each(db, docs, (c) => removeDocuments(db, c)),
      each(db, `SELECT id FROM submissions WHERE conversation_id IN (${convs})`, (c) =>
        removeRows(db, 'submissions', c),
      ),
      each(db, `SELECT id FROM entries WHERE conversation_id IN (${convs})`, (c) => removeRows(db, 'entries', c)),
    ]) {
      const n = step()
      if (n > 0) return n
    }
    const rows = batch.map((m) => Number(m.r)).join(',')
    if (removing) {
      removeRows(db, 'tasks', select(db, `SELECT id FROM tasks WHERE id IN (${tasks})`))
      removeRows(db, 'conversations', select(db, `SELECT id FROM conversations WHERE id IN (${convs})`))
      db.exec(`UPDATE taskto_purge_marks SET done = 1 WHERE rowid IN (${rows})`)
    } else {
      db.exec(`DELETE FROM taskto_purge_marks WHERE rowid IN (${rows})`)
    }
    return batch.length
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
export type PurgeJob = { path: string; timeoutMs: number; root?: number }

type Outcome = { ok: true } | { ok: false; busy: boolean; message: string }

const MARK = 'taskto.purge'

function run(job: PurgeJob): Outcome {
  try {
    if (job.root === undefined) truncateWalAt(job.path, job.timeoutMs)
    else purgeRecords(job.path, job.root, job.timeoutMs)
    return { ok: true }
  } catch (error) {
    return { ok: false, busy: error instanceof WalBusyError, message: (error as Error).message ?? String(error) }
  }
}

/**
 * Runs job on a worker thread of its own and settles when it is done: rejects with WalBusyError when
 * a reader held the log, and with an Error for any other failure. The calling thread is free
 * meanwhile.
 */
export function runPurge(job: PurgeJob): Promise<void> {
  return new Promise((resolve, reject) => {
    let settled = false
    const worker = new Worker(new URL(import.meta.url), { workerData: { [MARK]: job } })
    worker.once('message', (out: Outcome) => {
      settled = true
      if (out.ok) resolve()
      else reject(out.busy ? new WalBusyError(out.message) : new Error(out.message))
    })
    worker.once('error', (error) => {
      if (settled) return
      settled = true
      reject(error)
    })
    worker.once('exit', (code) => {
      if (settled) return
      settled = true
      reject(new Error(`the purge's worker thread exited with code ${code}`))
    })
  })
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

/**
 * Copies the log of the database at path into the database by a passive checkpoint every everyMs,
 * on a worker thread of its own (writers go on beside it, and it never waits for readers), until the
 * function it returns is called. It stands in for the commits' own checkpoints, which
 * HARNESS_DB_OPTIONS turns off. A checkpoint that fails is logged and counted; a thread whose
 * checkpoints keep failing, or that fails, is started again.
 */
export function startCheckpointer(path: string, everyMs = CHECKPOINT_EVERY_MS): () => Promise<void> {
  const stop = new Int32Array(new SharedArrayBuffer(4))
  let worker: Worker
  const start = () => {
    worker = new Worker(new URL(import.meta.url), { workerData: { [CHECKPOINTER]: { path, everyMs, stop } } })
    worker.unref()
    worker.on('error', (error) => console.error('the checkpointer failed; it starts again', error))
    worker.once('exit', () => {
      if (Atomics.load(stop, 0) === 0) setTimeout(start, everyMs).unref()
    })
  }
  start()
  return async () => {
    Atomics.store(stop, 0, 1)
    Atomics.notify(stop, 0)
    await new Promise<void>((resolve) => {
      worker.once('exit', () => resolve())
      // Already gone: nothing to wait for.
      if (worker.threadId === -1) resolve()
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
        passive(db)
        failures = 0
      } catch (error) {
        failures++
        failed++
        console.error(`checkpoint failed (${failed} so far, ${failures} in a row): ${(error as Error).message}`)
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
