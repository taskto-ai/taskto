// How the sidecar opens Pi Durable's database, and the one-time compaction of a database from before
// it did so (docs/install.md#privacy-and-account-deletion).
//
// Pi Durable deletes rows on its own connection as it goes: a document's earlier revisions when a
// new one is written (a queue of input whose items were withdrawn, for one), and more. SQLite leaves
// what a deleted row held in the file, in the page's free space or on the free list, unless the
// connection that deletes it sets secure_delete; so the harness's connection sets it, as the purge's
// does (purge.ts), and what anyone deletes is overwritten with zeros. A database written before that
// may still hold such remnants, which only a VACUUM (a rewrite of every page) removes: compactOnce.
import { existsSync, statfsSync, statSync } from 'node:fs'
import { dirname } from 'node:path'
import { DatabaseSync } from 'node:sqlite'
import { SqliteStorage } from '@earendil-works/pi-durable/storage/sqlite'
import { openNodeSqliteDatabase } from '@earendil-works/pi-durable/storage/sqlite/node'
import { HARNESS_DB_OPTIONS } from './purge.js'

/**
 * Opens the harness's storage at path as openNodeSqliteStorage does (with HARNESS_DB_OPTIONS), and
 * with secure_delete on its connection: what Pi Durable deletes is overwritten with zeros in the file.
 * ON rather than FAST: FAST leaves a page it frees as it was, and large content (an input, a
 * document's revision) lives in overflow pages, which are freed when it is deleted.
 */
export async function openHarnessStorage(path: string): Promise<SqliteStorage> {
  const db = await openNodeSqliteDatabase(path, HARNESS_DB_OPTIONS)
  await db.exec('PRAGMA secure_delete = ON')
  return SqliteStorage.open(db)
}

/**
 * The largest database compactOnce compacts at start: well inside the time the server gives a new
 * sidecar to answer (30 s) on a slow disk. A larger one is left as it is, and the log says so.
 */
export const COMPACT_MAX_BYTES = 256 * 1024 * 1024

/**
 * What compactOnce did. deferred: it couldn't be finished now (a reader held the log, so it couldn't
 * be emptied, or SQLite failed: the disk full, the file read-only); it is tried again at the next
 * start, and nothing is recorded.
 */
export type Compaction = 'fresh' | 'done-before' | 'compacted' | 'too-large' | 'no-room' | 'deferred'

// The same record as `taskto agent-compact` writes (internal/agentrt/compact.go; a test there checks
// the two agree).
const MAINTENANCE_TABLE = `CREATE TABLE IF NOT EXISTS taskto_maintenance (
  name    TEXT PRIMARY KEY,
  done_at INTEGER NOT NULL
) STRICT`

const COMPACTED = 'secure_delete_vacuum'

/**
 * Compacts the database at path once (VACUUM), before the harness opens it, so nothing else writes
 * meanwhile: a database written before the harness's connection set secure_delete may hold what was
 * deleted then, and the rewrite leaves none of it. It is recorded in the sidecar's own table
 * (taskto_maintenance) and not done again. A new database (no file yet) needs none, and is recorded
 * by recordFresh once the harness has made it. It is bounded: not for a database larger than
 * maxBytes, which would hold the sidecar's start past the server's patience, nor without room on the
 * disk for the copy SQLite makes (twice its size); either is left for the next start, and logged.
 *
 * It is recorded done only once the rewrite is in the file and the log is empty: a reader that holds
 * the log (a backup's snapshot) keeps what was deleted in it, and the TRUNCATE checkpoint that would
 * empty it says busy. Then it is deferred, as it is when SQLite fails meanwhile; the sidecar starts
 * all the same. Whether anything is to do is read on a read-only connection, whose closing never
 * checkpoints the log: after a hard stop that can be large, and the start would wait for it.
 */
export function compactOnce(path: string, maxBytes = COMPACT_MAX_BYTES): Compaction {
  if (!existsSync(path)) return 'fresh'
  // The look and the disk's room are part of the compaction too: a failure of either defers it,
  // logged, as one of the rewrite does, and the sidecar starts all the same.
  try {
    return compactIfDue(path, maxBytes)
  } catch (error) {
    console.error(`compacting agent.db is deferred to the next start: ${(error as Error).message}`)
    return 'deferred'
  }
}

function compactIfDue(path: string, maxBytes: number): Compaction {
  let size: number
  const look = new DatabaseSync(path, { readOnly: true })
  try {
    const recorded = look
      .prepare("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'taskto_maintenance'")
      .get()
    if (recorded && look.prepare('SELECT 1 FROM taskto_maintenance WHERE name = ?').get(COMPACTED)) return 'done-before'
    const pages = Number((look.prepare('PRAGMA page_count').get() as { page_count: number }).page_count)
    const pageSize = Number((look.prepare('PRAGMA page_size').get() as { page_size: number }).page_size)
    let wal = 0
    try {
      wal = statSync(`${path}-wal`).size
    } catch {
      // no log: nothing to add
    }
    size = pages * pageSize + wal
  } finally {
    look.close()
  }
  if (size > maxBytes) {
    console.error(
      `agent.db is ${size} bytes, more than the ${maxBytes} compacted at start: content deleted before this version may remain in the file until it is compacted: stop the server and its backups and run taskto agent-compact (docs/install.md#privacy-and-account-deletion)`,
    )
    return 'too-large'
  }
  const disk = statfsSync(dirname(path))
  if (Number(disk.bavail) * Number(disk.bsize) < 2 * size) {
    console.error('not enough room on the disk to compact agent.db at start; it is tried again at the next start')
    return 'no-room'
  }
  let db: DatabaseSync | undefined
  try {
    db = new DatabaseSync(path)
    db.exec(MAINTENANCE_TABLE)
    db.exec('PRAGMA secure_delete = ON')
    db.exec('VACUUM')
    const row = db.prepare('PRAGMA wal_checkpoint(TRUNCATE)').get() as { busy: number | bigint } | undefined
    if (Number(row?.busy ?? 1) !== 0) {
      console.error(
        'compacting agent.db is deferred to the next start: a reader holds its log, so it could not be emptied',
      )
      return 'deferred'
    }
    db.prepare('INSERT OR REPLACE INTO taskto_maintenance (name, done_at) VALUES (?, ?)').run(COMPACTED, Date.now())
    return 'compacted'
  } catch (error) {
    console.error(`compacting agent.db is deferred to the next start: ${(error as Error).message}`)
    return 'deferred'
  } finally {
    db?.close()
  }
}

/** Records a new database as needing no compaction (compactOnce), once the harness has made it. */
export function recordFresh(path: string): void {
  const db = new DatabaseSync(path, { timeout: 5000 })
  try {
    db.exec(MAINTENANCE_TABLE)
    db.prepare('INSERT OR IGNORE INTO taskto_maintenance (name, done_at) VALUES (?, ?)').run(COMPACTED, Date.now())
  } finally {
    db.close()
  }
}
