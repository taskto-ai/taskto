import { existsSync, statfsSync, statSync } from 'node:fs'
import { dirname } from 'node:path'
import { DatabaseSync } from 'node:sqlite'
import { SqliteStorage } from '@earendil-works/pi-durable/storage/sqlite'
import { openNodeSqliteDatabase } from '@earendil-works/pi-durable/storage/sqlite/node'
import { HARNESS_DB_OPTIONS } from './purge.js'

export async function openHarnessStorage(path: string): Promise<SqliteStorage> {
  const db = await openNodeSqliteDatabase(path, HARNESS_DB_OPTIONS)
  await db.exec('PRAGMA secure_delete = ON')
  return SqliteStorage.open(db)
}

export const COMPACT_MAX_BYTES = 256 * 1024 * 1024

export type Compaction = 'fresh' | 'done-before' | 'compacted' | 'too-large' | 'no-room' | 'deferred'

const MAINTENANCE_TABLE = `CREATE TABLE IF NOT EXISTS taskto_maintenance (
  name    TEXT PRIMARY KEY,
  done_at INTEGER NOT NULL
) STRICT`

const COMPACTED = 'secure_delete_vacuum'

export function compactOnce(path: string, maxBytes = COMPACT_MAX_BYTES): Compaction {
  if (!existsSync(path)) return 'fresh'
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
      wal = 0
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

export function recordFresh(path: string): void {
  const db = new DatabaseSync(path, { timeout: 5000 })
  try {
    db.exec(MAINTENANCE_TABLE)
    db.prepare('INSERT OR IGNORE INTO taskto_maintenance (name, done_at) VALUES (?, ?)').run(COMPACTED, Date.now())
  } finally {
    db.close()
  }
}
