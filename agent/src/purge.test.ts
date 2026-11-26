import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { DatabaseSync } from 'node:sqlite'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { purgeRecords, runPurge, WalBusyError } from './purge.js'

const WORD = 'doomed-marmalade-7731'

// How often the main thread got to run a timer while fn's promise was pending (or while fn ran,
// for a synchronous fn), and how long that took.
async function ticksDuring(fn: () => unknown): Promise<{ ticks: number; ms: number; error?: unknown }> {
  let ticks = 0
  const timer = setInterval(() => {
    ticks++
  }, 10)
  const t0 = performance.now()
  let error: unknown
  try {
    await fn()
  } catch (e) {
    error = e
  }
  const ms = performance.now() - t0
  clearInterval(timer)
  return { ticks, ms, error }
}

describe('runPurge', () => {
  let dir: string
  let path: string
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'taskto-purge-'))
    path = join(dir, 'agent.db')
  })
  afterEach(() => rmSync(dir, { recursive: true, force: true }))

  const log = () => {
    try {
      return readFileSync(`${path}-wal`).toString('latin1')
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') return ''
      throw error
    }
  }

  // The tables of Pi Durable's database the purge touches, with conversation 1 (said WORD) and
  // conversation 2 (someone else's), and a reader pinned on that state, as a watch or a harness's
  // read is.
  const fixture = () => {
    const writer = new DatabaseSync(path, { timeout: 100 })
    writer.exec('PRAGMA journal_mode = WAL')
    writer.exec('PRAGMA wal_autocheckpoint = 0')
    writer.exec(`
      CREATE TABLE conversations (id INTEGER PRIMARY KEY, owner_task_id INTEGER);
      CREATE TABLE tasks (id INTEGER PRIMARY KEY, conversation_id INTEGER);
      CREATE TABLE entries (id INTEGER PRIMARY KEY, conversation_id INTEGER, text TEXT);
      CREATE TABLE submissions (id INTEGER PRIMARY KEY, conversation_id INTEGER);
      CREATE TABLE documents (id INTEGER PRIMARY KEY, scope_kind TEXT, owner_id INTEGER);
      CREATE TABLE document_revisions (id INTEGER PRIMARY KEY, document_id INTEGER);
      CREATE TABLE record_ids (id INTEGER PRIMARY KEY);
      INSERT INTO conversations (id) VALUES (1), (2);
      INSERT INTO record_ids (id) VALUES (1), (2), (10), (11);
    `)
    writer.prepare('INSERT INTO entries (id, conversation_id, text) VALUES (10, 1, ?), (11, 2, ?)').run(WORD, 'kept')
    const reader = new DatabaseSync(path, { timeout: 100 })
    reader.exec('BEGIN')
    reader.prepare('SELECT count(*) FROM entries').get()
    return { writer, reader }
  }

  it('waits for a reader off the calling thread, which keeps running meanwhile', async () => {
    const { writer, reader } = fixture()
    const wait = 600
    try {
      // On the calling thread (as the sidecar did), the wait for the reader stalls it: no timer runs.
      const inline = await ticksDuring(() => purgeRecords(path, 2, wait))
      expect(inline.error).toBeInstanceOf(WalBusyError)
      expect(inline.ms).toBeGreaterThanOrEqual(wait * 0.8)
      expect(inline.ticks).toBe(0)

      // On a worker thread: the same wait, and the calling thread's timers keep running.
      const off = await ticksDuring(() => runPurge({ path, root: 1, timeoutMs: wait }))
      expect(off.error).toBeInstanceOf(WalBusyError) // the purge isn't done while the reader holds the log
      expect(off.ms).toBeGreaterThanOrEqual(wait * 0.8)
      expect(off.ticks).toBeGreaterThanOrEqual(Math.floor(wait / 10 / 3))
      expect(log()).toContain(WORD)

      // The reader lets go: the retry, which finds the rows gone, empties the log.
      reader.exec('COMMIT')
      await runPurge({ path, timeoutMs: wait })
      expect(log()).not.toContain(WORD)
      expect(writer.prepare('SELECT id FROM conversations ORDER BY id').all()).toEqual([])
      expect(writer.prepare('SELECT count(*) AS n FROM entries').get()).toEqual({ n: 0 })
    } finally {
      reader.close()
      writer.close()
    }
  })

  it('purges one conversation and leaves the others', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    try {
      await runPurge({ path, root: 1, timeoutMs: 1000 })
      expect(writer.prepare('SELECT id FROM conversations').all()).toEqual([{ id: 2 }])
      expect(writer.prepare('SELECT id, text FROM entries').all()).toEqual([{ id: 11, text: 'kept' }])
      expect(writer.prepare('SELECT id FROM record_ids ORDER BY id').all()).toEqual([{ id: 2 }, { id: 11 }])
      expect(log()).not.toContain(WORD)
    } finally {
      reader.close()
      writer.close()
    }
  })

  it('rejects with the failure of a purge that is not about the log', async () => {
    await expect(runPurge({ path: join(dir, 'missing', 'agent.db'), root: 1, timeoutMs: 100 })).rejects.toThrow()
  })
})
