// The SQLite work of a purge: deleting a retired conversation's records from the sidecar's database,
// and emptying the write-ahead log after it.
//
// Pi Durable has no way to delete a conversation, so this is a second connection to its database
// (WAL: it waits for the harness's writes, and the harness for its). node:sqlite is synchronous, and
// a purge can wait up to its busy timeout for the write lock or for a reader that pins the log; on
// the HTTP server's thread that wait would stall every other conversation's turn and /healthz. So
// the sidecar runs it on a worker thread of its own (runPurge), and the main thread only awaits it.
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

/** Truncates the log of the database at path, on a connection of its own that waits up to timeoutMs. */
export function truncateWalAt(path: string, timeoutMs: number): void {
  const db = new DatabaseSync(path, { timeout: timeoutMs })
  try {
    truncateWal(db)
  } finally {
    db.close()
  }
}

// Deletes a conversation's records, and those of the conversations its tasks started, from the
// database at path: its entries (what was said), tasks, submissions and documents with their
// revisions. Only a retired conversation is purged: its work has stopped, so nothing writes the rows
// that go. secure_delete zeroes the deleted content in the file, and the checkpoint takes it out of
// the write-ahead log.
export function purgeRecords(path: string, root: number, timeoutMs: number): void {
  const db = new DatabaseSync(path, { timeout: timeoutMs })
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
