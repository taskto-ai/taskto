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

import { readFileSync } from 'node:fs'
import { endianness } from 'node:os'
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
 * How many frames of the log may still be waiting to be copied when the purge empties it: the copy
 * is done under the write lock, so it is only tried on a log that a passive checkpoint has all but
 * drained (what the harness wrote since).
 */
const DRAINED_FRAMES = 64

/**
 * Copies what it can of the log into the database by a checkpoint that never waits and never holds
 * the harness's writes up (PASSIVE), and says how many frames are left to copy: some while a
 * reader's snapshot still needs them. Infinity when it couldn't run.
 */
function backlog(db: DatabaseSync): number {
  try {
    const row = db.prepare('PRAGMA wal_checkpoint(PASSIVE)').get() as
      | { log: number | bigint; checkpointed: number | bigint }
      | undefined
    if (!row) return Number.POSITIVE_INFINITY
    const log = Number(row.log)
    return log < 0 ? 0 : log - Number(row.checkpointed) // -1: not in WAL mode, nothing to copy
  } catch {
    return Number.POSITIVE_INFINITY
  }
}

/**
 * The frames of the log at path that no checkpoint has copied yet, read from SQLite's WAL index
 * (path-shm, https://www.sqlite.org/walformat.html): the header's mxFrame (offset 16) less the
 * checkpoint's nBackfill (offset 96), both in the machine's byte order. Reading it copies nothing.
 * Infinity when the index can't be read as one.
 */
export function pendingFrames(path: string): number {
  let shm: Buffer
  try {
    shm = readFileSync(`${path}-shm`).subarray(0, 136)
  } catch {
    return Number.POSITIVE_INFINITY
  }
  if (shm.length < 136) return Number.POSITIVE_INFINITY
  const le = endianness() === 'LE'
  const u32 = (at: number) => (le ? shm.readUInt32LE(at) : shm.readUInt32BE(at))
  if (u32(0) !== 3007000) return Number.POSITIVE_INFINITY // the WAL index's version
  const pending = u32(16) - u32(96)
  return pending < 0 ? Number.POSITIVE_INFINITY : pending
}

/** What truncateWalWithin tells a test. */
export type TruncateHooks = {
  /** Each round's backlog after its passive checkpoint, before the round decides what to do. */
  round?: (backlog: number) => void
  /** Just before the TRUNCATE checkpoint, the frames it has to copy, as read under the write lock. */
  truncating?: (pending: number) => void
}

/**
 * truncateWal, tried until timeoutMs has passed, never holding the write lock while it waits or
 * copies much: each round copies the log by a passive checkpoint (which writers go on beside), and
 * only once it is drained tries the TRUNCATE, which holds the lock while it runs (busy_timeout 0, so
 * it doesn't wait for readers either). The pause between rounds is a sleep of this thread. A reader
 * that lets go of its snapshot lets the next round copy the rest. Throws WalBusyError if a reader
 * still holds the log at the end.
 *
 * A commit can land between the passive checkpoint and the TRUNCATE (one that was being written while
 * the passive one ran), which the TRUNCATE would then copy under the lock. So the drained log is
 * checked again while this thread holds the write lock (on a second connection, lock), from the WAL
 * index, which copies nothing, and the TRUNCATE is tried the moment the lock is let go: a writer that
 * takes it first makes the TRUNCATE busy for this round, and none can write much in between.
 */
export function truncateWalWithin(db: DatabaseSync, path: string, timeoutMs: number, hooks?: TruncateHooks): void {
  db.exec('PRAGMA busy_timeout = 0')
  const lock = new DatabaseSync(path, { timeout: 0 })
  try {
    const deadline = performance.now() + timeoutMs
    let last: unknown
    for (;;) {
      const left = backlog(db)
      hooks?.round?.(left)
      if (left <= DRAINED_FRAMES && drainedUnderLock(lock, path, hooks)) {
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
  } finally {
    lock.close()
  }
}

// Whether the log at path is drained while lock holds the write lock (false if a writer has it).
function drainedUnderLock(lock: DatabaseSync, path: string, hooks?: TruncateHooks): boolean {
  try {
    lock.exec('BEGIN IMMEDIATE')
  } catch {
    return false // a writer is at it: the next round looks again
  }
  let pending: number
  try {
    pending = pendingFrames(path)
  } finally {
    lock.exec('ROLLBACK')
  }
  if (pending > DRAINED_FRAMES) return false
  hooks?.truncating?.(pending)
  return true
}

/** Empties the log of the database at path, on a connection of its own, trying for up to timeoutMs. */
export function truncateWalAt(path: string, timeoutMs: number): void {
  const db = new DatabaseSync(path, { timeout: timeoutMs })
  try {
    truncateWalWithin(db, path, timeoutMs)
  } finally {
    db.close()
  }
}

/**
 * The kind of the sidecar's document that says when a conversation was created and retired
 * (index.ts, Lifecycle). The purge removes it last, with the conversation's row.
 */
export const LIFECYCLE_KIND = 'taskto.lifecycle'

/** How many rows one statement of the purge deletes: small, so a transaction stays near SLICE_MS. */
const CHUNK = 100

/** How long one of the purge's transactions goes on deleting before it commits and pauses. */
const SLICE_MS = 20

// Deletes a conversation's records, and those of the conversations its tasks started, from the
// database at path: its entries (what was said), tasks, submissions and documents with their
// revisions, each record with its row of record_ids. Only a retired conversation is purged: its work
// has stopped, so nothing writes the rows that go. secure_delete zeroes the deleted content in the
// file, and the checkpoint takes it out of the write-ahead log.
//
// Everything goes a chunk at a time, in short transactions (see the top of the file), in an order
// that lets a purge that stops halfway be finished by the next, which finds the tree again from the
// root: first what was said (documents with their revisions, submissions, entries), then the tree
// itself from the bottom up, a task only once no conversation it started is left and a conversation
// only once no task of it is left, so whatever is left is still reachable from the root. A
// conversation's records that were written late go with it, and a document's revisions with it, in
// the same transaction. The root's row and its lifecycle document, which says it is retired (so the
// purge goes on, and it stays closed to new input), go last, in a transaction that finds the tree
// again and takes whatever is left: normally the root alone, so it is short, and nothing written
// during the pauses outlives a purge that says it is done.
//
// afterSlice, for tests, runs after each short transaction commits.
export function purgeRecords(path: string, root: number, timeoutMs: number, afterSlice?: () => void): void {
  const db = new DatabaseSync(path, { timeout: timeoutMs })
  try {
    db.exec('PRAGMA secure_delete = ON')
    // As the harness's own connection: a commit doesn't wait for the disk, which would hold the
    // write lock for its sync. The checkpoint that ends the purge syncs the database, and the purge
    // isn't done (the server asks again) until it has.
    db.exec('PRAGMA synchronous = NORMAL')
    // Every conversation and task of the tree is remembered before any of their rows go: a retry
    // after a stop finds the tree again from them too, so a record written for a conversation whose
    // row went (a child's late entry) is found. Pi Durable never gives an id again (its next_id only
    // grows), so a remembered id names nothing else.
    db.exec(REMEMBERED_TABLE)
    const seen = discover(db, root, remembered(db, root))
    remember(db, root, seen)
    const steps = purgeSteps(db, root, seen)
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
      afterSlice?.()
      sleep(PAUSE_MS)
    }
    db.exec('BEGIN IMMEDIATE')
    try {
      // Found again under the write lock, from what this purge removed too: what was written during
      // the pauses goes, even into a conversation whose row went in an earlier transaction.
      const tree = discover(db, root, seen)
      for (const step of purgeSteps(db, root, tree)) while (step() > 0);
      const ids = list(tree.ids)
      removeDocuments(db, select(db, `SELECT id FROM documents WHERE ${ownedBy(tree)}`))
      removeRows(db, 'tasks', select(db, `SELECT id FROM tasks WHERE conversation_id IN (${ids})`))
      removeRows(db, 'conversations', select(db, `SELECT id FROM conversations WHERE id IN (${ids})`))
      db.prepare('DELETE FROM taskto_purge_tree WHERE root = ?').run(root)
      db.exec('COMMIT')
    } catch (error) {
      db.exec('ROLLBACK')
      throw error
    }
    // What was deleted is still in the log until it is emptied, and a reader (the harness's, or a
    // watch) can hold it up: then the purge isn't done, and the server asks again.
    truncateWalWithin(db, path, timeoutMs)
  } finally {
    db.close()
  }
}

/**
 * The conversations and tasks of each purge that has begun and not ended, by its root: the
 * sidecar's own table in Pi Durable's database, which Pi Durable doesn't read. Its rows go in the
 * transaction that ends the purge.
 */
const REMEMBERED_TABLE = `CREATE TABLE IF NOT EXISTS taskto_purge_tree (
  root INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('conversation', 'task')),
  id   INTEGER NOT NULL,
  PRIMARY KEY (root, kind, id)
) STRICT`

/** How many ids one of remember's transactions writes. */
const REMEMBER_CHUNK = 2000

// The tree an earlier, unfinished purge of root remembered.
function remembered(db: DatabaseSync, root: number): Tree {
  const tree: Tree = { ids: new Set(), taskIds: new Set() }
  for (const row of db.prepare('SELECT kind, id FROM taskto_purge_tree WHERE root = ?').all(root) as {
    kind: string
    id: number | bigint
  }[]) {
    ;(row.kind === 'task' ? tree.taskIds : tree.ids).add(Number(row.id))
  }
  return tree
}

// Remembers tree's ids for root, in short transactions with pauses between them.
function remember(db: DatabaseSync, root: number, tree: Tree): void {
  const all = [
    ...[...tree.ids].map((id) => ['conversation', id] as const),
    ...[...tree.taskIds].map((id) => ['task', id] as const),
  ]
  for (let i = 0; i < all.length; i += REMEMBER_CHUNK) {
    const chunk = all.slice(i, i + REMEMBER_CHUNK)
    db.exec('BEGIN IMMEDIATE')
    try {
      const values = chunk.map(([kind, id]) => `(${root}, '${kind}', ${id})`).join(', ')
      db.exec(`INSERT OR IGNORE INTO taskto_purge_tree (root, kind, id) VALUES ${values}`)
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

/**
 * The steps that delete tree's rows but the root's own row and lifecycle document, each a chunk at
 * a time in the transaction that is open, saying how many rows it removed (0: that step is done).
 */
function purgeSteps(db: DatabaseSync, root: number, tree: Tree): (() => number)[] {
  const ids = list(tree.ids)
  const docs = `SELECT id FROM documents WHERE ${ownedBy(tree)} AND kind <> '${LIFECYCLE_KIND}'`
  const each = (query: string, remove: (chunk: number[]) => void) => () => {
    const chunk = select(db, `${query} LIMIT ${CHUNK}`)
    remove(chunk)
    return chunk.length
  }
  // A conversation of the tree that is about to go, with whatever of its was written late.
  const removeConversations = (chunk: number[]) => {
    const of = list(chunk)
    removeDocuments(
      db,
      select(db, `SELECT id FROM documents WHERE scope_kind = 'conversation' AND owner_id IN (${of})`),
    )
    removeRows(db, 'submissions', select(db, `SELECT id FROM submissions WHERE conversation_id IN (${of})`))
    removeRows(db, 'entries', select(db, `SELECT id FROM entries WHERE conversation_id IN (${of})`))
    removeRows(db, 'conversations', chunk)
  }
  return [
    each(`SELECT rowid AS id FROM document_revisions WHERE document_id IN (${docs})`, (c) =>
      removeRows(db, 'document_revisions', c, false),
    ),
    each(docs, (c) => removeDocuments(db, c)),
    each(`SELECT id FROM submissions WHERE conversation_id IN (${ids})`, (c) => removeRows(db, 'submissions', c)),
    each(`SELECT id FROM entries WHERE conversation_id IN (${ids})`, (c) => removeRows(db, 'entries', c)),
    // The tree from the bottom up: tasks that started no conversation still there, then the
    // conversations (but the root) that have no task left, until neither is left.
    () => {
      const tasks = select(
        db,
        `SELECT id FROM tasks WHERE conversation_id IN (${ids})
          AND NOT EXISTS (SELECT 1 FROM conversations c WHERE c.owner_task_id = tasks.id) LIMIT ${CHUNK}`,
      )
      if (tasks.length > 0) {
        removeDocuments(
          db,
          select(db, `SELECT id FROM documents WHERE scope_kind = 'task' AND owner_id IN (${list(tasks)})`),
        )
        removeRows(db, 'tasks', tasks)
        return tasks.length
      }
      const conversations = select(
        db,
        `SELECT id FROM conversations WHERE id IN (${ids}) AND id <> ${root}
          AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.conversation_id = conversations.id) LIMIT ${CHUNK}`,
      )
      removeConversations(conversations)
      return conversations.length
    },
  ]
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

const CHECKPOINTER = 'taskto.checkpointer'

type CheckpointerJob = { path: string; everyMs: number; stop: Int32Array }

/**
 * Copies the log of the database at path into the database by a passive checkpoint every everyMs,
 * on a worker thread of its own (writers go on beside it, and it never waits for readers), until the
 * function it returns is called. It stands in for the commits' own checkpoints, which
 * HARNESS_DB_OPTIONS turns off. A thread that fails is started again.
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
  try {
    while (Atomics.load(job.stop, 0) === 0) {
      backlog(db)
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
