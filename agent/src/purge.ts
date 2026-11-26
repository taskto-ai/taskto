// The purge of a conversation's records from the sidecar's database (POST /conversations/:id/purge).
// It is a lot of synchronous SQLite work: a long conversation has thousands of records, and the
// statements wait up to the busy timeout for the harness's writes and for readers of the log. Run on
// the main thread it would stop every turn, stream and request for as long as it takes, so the
// sidecar runs it on a worker thread (purgeOffThread) and the event loop goes on. The functions
// here are synchronous and know nothing of threads, so they are tested on their own.

import { DatabaseSync } from 'node:sqlite'
import { Worker } from 'node:worker_threads'
import { truncateWalAt, truncateWalWithin, WalBusyError } from './wal.js'

/** How long one transaction of a purge should take: the write lock is held that long at a time. */
export const BATCH_TARGET_MS = 20
/** The most rows one transaction deletes, and the fewest. A batch starts small and follows how long the last took. */
export const BATCH_MAX_ROWS = 500
export const BATCH_MIN_ROWS = 1
/** How long the connection rests between transactions, with the write lock released, so a waiting writer gets it. */
export const BATCH_PAUSE_MS = 5

const pause = (ms: number) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms)

// Deletes a conversation's records, and those of the conversations its tasks started, from the
// database: its entries (what was said), tasks, submissions and documents with their revisions.
// Pi Durable has no way to delete a conversation, so this is a second connection to its database
// (WAL: it waits for the harness's writes, and the harness for its). Only a retired conversation
// is purged: its work has stopped, so nothing writes the rows that go. secure_delete zeroes the
// deleted content in the file, and the checkpoint takes it out of the write-ahead log.
//
// The harness's own connection waits for the write lock inside SQLite's busy handler, on the thread
// that serves requests, which no code here can make asynchronous. So the purge never holds the lock
// for long: the records that hold content (document revisions, documents, submissions, entries) go
// in batches sized to take about BATCH_TARGET_MS, each its own transaction, with the lock released
// between them. The
// purge is still all or nothing for the caller: the rows that name the conversation and its
// sub-conversations (the conversations and their tasks) go last, in one more short transaction, and
// the sidecar answers that a conversation that still has its row isn't purged. An interrupted purge
// (the sidecar stops, a statement fails) leaves the conversation, retired and with fewer records,
// and the server asks again, which finds the same conversation and goes on from what is left. What
// a batch deleted from the log of record ids goes in the same transaction, so none is left behind.
export function purgeRecords(dbPath: string, root: number, walBusyMs: number): void {
  const db = new DatabaseSync(dbPath, { timeout: walBusyMs })
  try {
    db.exec('PRAGMA secure_delete = ON')
    const list = (set: Set<number>) => [...set].join(',') || 'NULL'
    const ids = new Set<number>([root])
    const taskIds = new Set<number>()
    const transaction = <T>(work: () => T): T => {
      db.exec('BEGIN IMMEDIATE')
      try {
        const result = work()
        db.exec('COMMIT')
        return result
      } catch (error) {
        db.exec('ROLLBACK')
        throw error
      }
    }
    // The conversations the root's tasks started, and theirs in turn. They and their tasks stay
    // until the last transaction, so a retry finds them again.
    transaction(() => {
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
    })
    const docs = `SELECT id FROM documents WHERE (scope_kind = 'conversation' AND owner_id IN (${list(ids)}))
      OR (scope_kind = 'task' AND owner_id IN (${list(taskIds)}))`
    // Runs `batch(limit)` in transactions until it finds fewer rows than the limit, resting between.
    // Rows differ a lot in size (one entry can hold a whole tool result), so the limit follows how
    // long the last transaction took, to keep each near BATCH_TARGET_MS whatever the rows hold.
    let limit = 16
    const inBatches = (batch: (limit: number) => number) => {
      for (;;) {
        const asked = limit
        const started = performance.now()
        const found = transaction(() => batch(asked))
        const took = performance.now() - started
        if (found < asked) return
        if (took > BATCH_TARGET_MS) limit = Math.max(BATCH_MIN_ROWS, Math.floor(asked / 2))
        else if (took < BATCH_TARGET_MS / 2) limit = Math.min(BATCH_MAX_ROWS, asked * 2)
        pause(BATCH_PAUSE_MS)
      }
    }
    // Revisions have no id of their own: they go by their document.
    inBatches((n) => {
      const rows = db
        .prepare(`SELECT rowid AS r FROM document_revisions WHERE document_id IN (${docs}) LIMIT ${n}`)
        .all() as { r: number }[]
      if (rows.length > 0)
        db.exec(`DELETE FROM document_revisions WHERE rowid IN (${rows.map((r) => Number(r.r)).join(',')})`)
      return rows.length
    })
    // The records with an id: deleted with their record ids, in the same transaction.
    const byId = (table: string, select: string) =>
      inBatches((n) => {
        const found = (db.prepare(`${select} LIMIT ${n}`).all() as { id: number }[]).map((r) => Number(r.id))
        if (found.length > 0) {
          db.exec(`DELETE FROM ${table} WHERE id IN (${found.join(',')})`)
          db.exec(`DELETE FROM record_ids WHERE id IN (${found.join(',')})`)
        }
        return found.length
      })
    byId('documents', docs)
    byId('submissions', `SELECT id FROM submissions WHERE conversation_id IN (${list(ids)})`)
    byId('entries', `SELECT id FROM entries WHERE conversation_id IN (${list(ids)})`)
    // What names the conversations goes last, so a purge that stopped before here is asked again.
    transaction(() => {
      db.exec(`DELETE FROM tasks WHERE conversation_id IN (${list(ids)})`)
      db.exec(`DELETE FROM conversations WHERE id IN (${list(ids)})`)
      // Deleted in chunks: a conversation tree may have more ids than a statement may name.
      const gone = [...ids, ...taskIds]
      for (let i = 0; i < gone.length; i += 500)
        db.exec(`DELETE FROM record_ids WHERE id IN (${gone.slice(i, i + 500).join(',')})`)
      return 0
    })
    // What was deleted is still in the log until it is truncated, and a reader (the harness's, or a
    // watch) can hold it up: then the purge isn't done, and the server asks again.
    truncateWalWithin(db, walBusyMs)
  } finally {
    db.close()
  }
}

/** What the worker thread is asked to do. */
export type PurgeJob =
  | { op: 'purge'; dbPath: string; root: number; walBusyMs: number }
  | { op: 'truncate'; dbPath: string; walBusyMs: number }

/** What the worker says when it ends: done, or why not (a reader holds the log, or any other failure). */
export type PurgeOutcome = { ok: true } | { ok: false; busy: boolean; message: string }

/** Does the job, synchronously, in this thread: what the worker thread runs. */
export function runPurgeJob(job: PurgeJob): void {
  if (job.op === 'purge') purgeRecords(job.dbPath, job.root, job.walBusyMs)
  else truncateWalAt(job.dbPath, job.walBusyMs)
}

/**
 * Runs the job on a worker thread of its own, one per purge (they are rare, and its connection to
 * the database is closed with it), and settles when it ends. A log a reader holds is a
 * WalBusyError, as in the thread; any other failure is an Error with its message.
 */
export function purgeOffThread(job: PurgeJob, worker = new URL('./purge-worker.js', import.meta.url)): Promise<void> {
  return new Promise((resolve, reject) => {
    const thread = new Worker(worker, { workerData: job })
    let settled = false
    const settle = (error?: Error) => {
      if (settled) return
      settled = true
      if (error) reject(error)
      else resolve()
    }
    thread.once('message', (outcome: PurgeOutcome) => {
      if (outcome.ok) settle()
      else settle(outcome.busy ? new WalBusyError(outcome.message) : new Error(outcome.message))
    })
    thread.once('error', (error) => settle(error instanceof Error ? error : new Error(String(error))))
    thread.once('exit', (code) => settle(new Error(`the purge worker stopped before it answered (exit ${code})`)))
  })
}
