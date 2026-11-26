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
// all the while), sleeping between them on its own thread.
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
 * truncateWal, tried until timeoutMs has passed, never holding the write lock while it waits: each
 * try holds it only while the checkpoint runs (busy_timeout 0), and the pause between tries is a
 * sleep of this thread. Throws WalBusyError if a reader still holds the log at the end.
 */
export function truncateWalWithin(db: DatabaseSync, timeoutMs: number): void {
  db.exec('PRAGMA busy_timeout = 0')
  // Most of the log is copied into the database first by a checkpoint that lets writers go on
  // meanwhile, so the one that empties it, which they wait for, has little left to do.
  db.prepare('PRAGMA wal_checkpoint(PASSIVE)').get()
  const deadline = performance.now() + timeoutMs
  for (;;) {
    try {
      truncateWal(db)
      return
    } catch (error) {
      if (!(error instanceof WalBusyError) || performance.now() + PAUSE_MS > deadline) throw error
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
 * (index.ts, Lifecycle). The purge removes it last, with the conversation's row.
 */
export const LIFECYCLE_KIND = 'taskto.lifecycle'

/** How many rows one statement of the purge deletes. */
const CHUNK = 500

/** How long one of the purge's transactions goes on deleting before it commits and pauses. */
const SLICE_MS = 20

// Deletes a conversation's records, and those of the conversations its tasks started, from the
// database at path: its entries (what was said), tasks, submissions and documents with their
// revisions, each record with its row of record_ids. Only a retired conversation is purged: its work
// has stopped, so nothing writes the rows that go. secure_delete zeroes the deleted content in the
// file, and the checkpoint takes it out of the write-ahead log.
//
// What was said (entries, submissions, documents) goes a chunk at a time, in short transactions (see
// the top of the file). What a purge that stops halfway needs to be finished by the next goes last,
// in one transaction: the tasks' and conversations' rows, by which the purge finds every
// conversation's records, and the lifecycle documents, which say the conversations are retired (so
// the purge goes on, and they stay closed to new input, until they are gone). Those are a few rows
// per conversation.
//
// afterSlice, for tests, runs after each short transaction commits.
export function purgeRecords(path: string, root: number, timeoutMs: number, afterSlice?: () => void): void {
  const db = new DatabaseSync(path, { timeout: timeoutMs })
  try {
    db.exec('PRAGMA secure_delete = ON')
    const ids = new Set<number>([root])
    const taskIds = new Set<number>()
    const list = (set: Set<number>) => [...set].join(',') || 'NULL'
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
    const owned = `((scope_kind = 'conversation' AND owner_id IN (${list(ids)}))
      OR (scope_kind = 'task' AND owner_id IN (${list(taskIds)})))`
    const docs = `SELECT id FROM documents WHERE ${owned} AND kind <> '${LIFECYCLE_KIND}'`
    const lifecycle = `SELECT id FROM documents WHERE ${owned} AND kind = '${LIFECYCLE_KIND}'`
    // Each step deletes the rows of a table that match, by rowid (a record's id), with their
    // record_ids rows when they are records (a document's revision isn't).
    type Step = { table: string; where: string; records: boolean }
    const steps: Step[] = [
      { table: 'document_revisions', where: `document_id IN (${docs})`, records: false },
      { table: 'documents', where: `id IN (${docs})`, records: true },
      { table: 'submissions', where: `conversation_id IN (${list(ids)})`, records: true },
      { table: 'entries', where: `conversation_id IN (${list(ids)})`, records: true },
    ]
    const last: Step[] = [
      { table: 'document_revisions', where: `document_id IN (${lifecycle})`, records: false },
      { table: 'documents', where: `id IN (${lifecycle})`, records: true },
      { table: 'tasks', where: `conversation_id IN (${list(ids)})`, records: true },
      { table: 'conversations', where: `id IN (${list(ids)})`, records: true },
    ]
    // A chunk of a step's rows, deleted in the transaction that is open: how many there were.
    const deleteChunk = (step: Step): number => {
      const chunk = (
        db.prepare(`SELECT rowid AS id FROM ${step.table} WHERE ${step.where} LIMIT ${CHUNK}`).all() as {
          id: number
        }[]
      ).map((r) => Number(r.id))
      if (chunk.length > 0) {
        db.exec(`DELETE FROM ${step.table} WHERE rowid IN (${chunk.join(',')})`)
        if (step.records) db.exec(`DELETE FROM record_ids WHERE id IN (${chunk.join(',')})`)
      }
      return chunk.length
    }
    // Transactions of at most about SLICE_MS each, with a pause between them in which the harness's
    // writes get the lock.
    for (let at = 0; at < steps.length; ) {
      db.exec('BEGIN IMMEDIATE')
      try {
        const start = performance.now()
        while (at < steps.length && performance.now() - start < SLICE_MS) {
          if (deleteChunk(steps[at]) < CHUNK) at++
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
      for (const step of last) while (deleteChunk(step) === CHUNK);
      db.exec('COMMIT')
    } catch (error) {
      db.exec('ROLLBACK')
      throw error
    }
    // What was deleted is still in the log until it is emptied, and a reader (the harness's, or a
    // watch) can hold it up: then the purge isn't done, and the server asks again.
    truncateWalWithin(db, timeoutMs)
  } finally {
    db.close()
  }
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

// The worker's side: only in a thread runPurge started, never on the sidecar's own thread or in a
// thread someone else (a test runner) started.
const mine = !isMainThread && (workerData as Record<string, unknown> | null)?.[MARK]
if (mine && parentPort) parentPort.postMessage(run(mine as PurgeJob))
