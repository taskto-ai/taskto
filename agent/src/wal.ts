// The write-ahead log of the sidecar's database after a purge. A deleted conversation's text can
// stay in the log until a checkpoint folds it in and truncates it, and SQLite reports a checkpoint
// that a reader held up as a result row (busy = 1) rather than an error, so the result is read.
import { DatabaseSync } from 'node:sqlite'

/** The log couldn't be truncated: a reader still holds it. The purge isn't done, and is asked again. */
export class WalBusyError extends Error {}

/** Folds the write-ahead log into the database file and empties it, or throws WalBusyError. */
export function truncateWal(db: DatabaseSync): void {
  let busy: number
  try {
    const row = db.prepare('PRAGMA wal_checkpoint(TRUNCATE)').get() as { busy: number | bigint } | undefined
    busy = Number(row?.busy ?? 1)
  } catch (error) {
    throw new WalBusyError(`the write-ahead log couldn't be checkpointed: ${(error as Error).message}`)
  }
  if (busy !== 0) throw new WalBusyError('a reader holds the write-ahead log, so it could not be truncated')
}

/** How long one checkpoint attempt waits for readers: while it waits it holds the database's write lock. */
const ATTEMPT_MS = 40
/** How long the connection rests between attempts, with the write lock released. */
const PAUSE_MS = 100

/**
 * Truncates the log, retrying for up to timeoutMs, or throws WalBusyError. A TRUNCATE checkpoint takes
 * the write lock and keeps it while it waits for readers, and the harness writes on the thread that
 * serves requests, so one long wait would hold up every write. Each attempt waits ATTEMPT_MS at most,
 * and the lock is free between attempts.
 */
export function truncateWalWithin(db: DatabaseSync, timeoutMs: number): void {
  const deadline = Date.now() + timeoutMs
  db.exec(`PRAGMA busy_timeout = ${ATTEMPT_MS}`)
  for (;;) {
    try {
      truncateWal(db)
      return
    } catch (error) {
      if (!(error instanceof WalBusyError) || Date.now() + PAUSE_MS >= deadline) throw error
    }
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, PAUSE_MS)
  }
}

/** Truncates the log of the database at path, on a connection of its own, retrying for up to timeoutMs. */
export function truncateWalAt(path: string, timeoutMs: number): void {
  const db = new DatabaseSync(path, { timeout: timeoutMs })
  try {
    truncateWalWithin(db, timeoutMs)
  } finally {
    db.close()
  }
}
