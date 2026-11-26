// The purge of a conversation's records from the sidecar's database (POST /conversations/:id/purge).
// It is a lot of synchronous SQLite work: a long conversation has thousands of records, and the
// statements wait up to the busy timeout for the harness's writes and for readers of the log. Run on
// the main thread it would stop every turn, stream and request for as long as it takes, so the
// sidecar runs it on a worker thread (purgeOffThread) and the event loop goes on. The functions
// here are synchronous and know nothing of threads, so they are tested on their own.

import { DatabaseSync } from 'node:sqlite'
import { Worker } from 'node:worker_threads'
import { truncateWal, truncateWalAt, WalBusyError } from './wal.js'

// Deletes a conversation's records, and those of the conversations its tasks started, from the
// database: its entries (what was said), tasks, submissions and documents with their revisions.
// Pi Durable has no way to delete a conversation, so this is a second connection to its database
// (WAL: it waits for the harness's writes, and the harness for its). Only a retired conversation
// is purged: its work has stopped, so nothing writes the rows that go. secure_delete zeroes the
// deleted content in the file, and the checkpoint takes it out of the write-ahead log.
export function purgeRecords(dbPath: string, root: number, walBusyMs: number): void {
  const db = new DatabaseSync(dbPath, { timeout: walBusyMs })
  try {
    db.exec('PRAGMA secure_delete = ON')
    db.exec('BEGIN IMMEDIATE')
    try {
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
      const docs = `SELECT id FROM documents WHERE (scope_kind = 'conversation' AND owner_id IN (${list(ids)}))
        OR (scope_kind = 'task' AND owner_id IN (${list(taskIds)}))`
      const records = [
        `SELECT id FROM entries WHERE conversation_id IN (${list(ids)})`,
        `SELECT id FROM submissions WHERE conversation_id IN (${list(ids)})`,
        docs,
      ]
      const gone = new Set<number>([...ids, ...taskIds])
      for (const q of records) for (const r of db.prepare(q).all() as { id: number }[]) gone.add(Number(r.id))
      db.exec(`DELETE FROM document_revisions WHERE document_id IN (${docs})`)
      db.exec(`DELETE FROM documents WHERE id IN (${docs})`)
      db.exec(`DELETE FROM submissions WHERE conversation_id IN (${list(ids)})`)
      db.exec(`DELETE FROM entries WHERE conversation_id IN (${list(ids)})`)
      db.exec(`DELETE FROM tasks WHERE conversation_id IN (${list(ids)})`)
      db.exec(`DELETE FROM conversations WHERE id IN (${list(ids)})`)
      // Deleted in chunks: a long conversation has more records than a statement may name.
      const all = [...gone]
      for (let i = 0; i < all.length; i += 500)
        db.exec(`DELETE FROM record_ids WHERE id IN (${all.slice(i, i + 500).join(',')})`)
      db.exec('COMMIT')
    } catch (error) {
      db.exec('ROLLBACK')
      throw error
    }
    // What was deleted is still in the log until it is truncated, and a reader (the harness's, or a
    // watch) can hold it up: then the purge isn't done, and the server asks again.
    truncateWal(db)
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
