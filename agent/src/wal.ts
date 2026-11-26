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

/** Truncates the log of the database at path, on a connection of its own that waits up to timeoutMs. */
export function truncateWalAt(path: string, timeoutMs: number): void {
  const db = new DatabaseSync(path, { timeout: timeoutMs })
  try {
    truncateWal(db)
  } finally {
    db.close()
  }
}
