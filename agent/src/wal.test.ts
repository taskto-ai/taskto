import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { DatabaseSync } from 'node:sqlite'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { truncateWal, truncateWalAt, WalBusyError } from './wal.js'

const WORD = 'doomed-marmalade-7731'

describe('truncateWal', () => {
  let dir: string
  let path: string
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'taskto-wal-'))
    path = join(dir, 'x.db')
  })
  afterEach(() => rmSync(dir, { recursive: true, force: true }))

  const log = () => {
    try {
      return readFileSync(`${path}-wal`).toString('latin1')
    } catch {
      return ''
    }
  }

  // A database that keeps what was said in the log, with a reader pinned on the state before the
  // deletion, as a watch or a harness's read is.
  const fixture = () => {
    const writer = new DatabaseSync(path, { timeout: 100 })
    writer.exec('PRAGMA journal_mode = WAL')
    writer.exec('PRAGMA wal_autocheckpoint = 0')
    writer.exec('CREATE TABLE entries (id INTEGER PRIMARY KEY, text TEXT)')
    writer.prepare('INSERT INTO entries (text) VALUES (?)').run(WORD)
    const reader = new DatabaseSync(path, { timeout: 100 })
    reader.exec('BEGIN')
    reader.prepare('SELECT count(*) FROM entries').get()
    writer.exec('DELETE FROM entries')
    return { writer, reader }
  }

  it('says it is not done while a reader holds the log, and empties it once none does', () => {
    const { writer, reader } = fixture()
    try {
      expect(() => truncateWal(writer)).toThrow(WalBusyError)
      expect(log()).toContain(WORD)
      reader.exec('COMMIT')
      truncateWal(writer)
      expect(log()).not.toContain(WORD)
    } finally {
      reader.close()
      writer.close()
    }
  })

  it('truncates on a connection of its own, which a retry after the rows are gone does', () => {
    const { writer, reader } = fixture()
    try {
      expect(() => truncateWalAt(path, 100)).toThrow(WalBusyError)
      reader.exec('COMMIT')
      truncateWalAt(path, 100)
      expect(log()).not.toContain(WORD)
    } finally {
      reader.close()
      writer.close()
    }
  })
})
