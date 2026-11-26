import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { DatabaseSync } from 'node:sqlite'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { LIFECYCLE_KIND, purgeRecords, runPurge, WalBusyError } from './purge.js'

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
      CREATE TABLE documents (id INTEGER PRIMARY KEY, kind TEXT NOT NULL DEFAULT 'notes', scope_kind TEXT, owner_id INTEGER);
      CREATE TABLE document_revisions (document_id INTEGER NOT NULL, seq INTEGER NOT NULL, content TEXT,
        PRIMARY KEY (document_id, seq));
      CREATE TABLE record_ids (id INTEGER PRIMARY KEY);
      -- Pi Durable's indexes of these, by which the purge finds the rows
      CREATE INDEX entries_by_conversation ON entries (conversation_id, id DESC);
      CREATE INDEX tasks_by_conversation ON tasks (conversation_id, id);
      CREATE INDEX conversations_by_owner_task ON conversations (owner_task_id, id);
      CREATE INDEX submissions_by_conversation ON submissions (conversation_id, id);
      CREATE INDEX documents_by_scope ON documents (scope_kind, owner_id, id);
      INSERT INTO conversations (id, owner_task_id) VALUES (1, NULL), (2, NULL), (3, 20);
      INSERT INTO tasks (id, conversation_id) VALUES (20, 1);
      INSERT INTO submissions (id, conversation_id) VALUES (30, 3);
      INSERT INTO documents (id, scope_kind, owner_id) VALUES (40, 'conversation', 1), (41, 'task', 20), (42, 'conversation', 2);
      INSERT INTO document_revisions (document_id, seq, content) VALUES (40, 1, 'a'), (40, 2, 'b'), (41, 1, 'c'), (42, 1, 'kept');
      -- Conversations 1 and 3 are retired, as the sidecar records it.
      INSERT INTO documents (id, kind, scope_kind, owner_id) VALUES (43, '${LIFECYCLE_KIND}', 'conversation', 1),
        (44, '${LIFECYCLE_KIND}', 'conversation', 3);
      INSERT INTO document_revisions (document_id, seq, content) VALUES (43, 1, 'retired'), (44, 1, 'retired');
      INSERT INTO record_ids (id) VALUES (1), (2), (3), (10), (11), (20), (30), (40), (41), (42), (43), (44);
    `)
    writer.prepare('INSERT INTO entries (id, conversation_id, text) VALUES (10, 1, ?), (11, 2, ?)').run(WORD, 'kept')
    const reader = new DatabaseSync(path, { timeout: 100 })
    reader.exec('BEGIN')
    reader.prepare('SELECT count(*) FROM entries').get()
    return { writer, reader }
  }

  // Writes on the calling thread, as the harness's commits are, every 15 ms while work is pending,
  // each on a connection that waits up to 5 s for the write lock: how long the longest one took.
  const writesDuring = async (work: Promise<unknown>) => {
    const db = new DatabaseSync(path, { timeout: 5000 })
    // Its own commits don't fold the log in: only how long it waits for the purge is measured.
    db.exec('PRAGMA wal_autocheckpoint = 0')
    const insert = db.prepare("INSERT INTO entries (conversation_id, text) VALUES (2, 'another turn')")
    let longest = 0
    let writes = 0
    let done = false
    const settled = work.then(
      () => undefined,
      (error: unknown) => error,
    )
    void settled.then(() => {
      done = true
    })
    while (!done) {
      const t0 = performance.now()
      insert.run()
      longest = Math.max(longest, performance.now() - t0)
      writes++
      await new Promise((r) => setTimeout(r, 15))
    }
    db.close()
    return { longest, writes, error: await settled }
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
      // Conversation 1 goes with its task's conversation (3), their submission and documents.
      expect(writer.prepare('SELECT id FROM conversations').all()).toEqual([{ id: 2 }])
      expect(writer.prepare('SELECT id, text FROM entries').all()).toEqual([{ id: 11, text: 'kept' }])
      expect(writer.prepare('SELECT id FROM tasks').all()).toEqual([])
      expect(writer.prepare('SELECT id FROM submissions').all()).toEqual([])
      expect(writer.prepare('SELECT id FROM documents').all()).toEqual([{ id: 42 }])
      expect(writer.prepare('SELECT document_id, content FROM document_revisions').all()).toEqual([
        { document_id: 42, content: 'kept' },
      ])
      expect(writer.prepare('SELECT id FROM record_ids ORDER BY id').all()).toEqual([{ id: 2 }, { id: 11 }, { id: 42 }])
      expect(log()).not.toContain(WORD)
    } finally {
      reader.close()
      writer.close()
    }
  })

  // While the purge waits for a reader to let go of the log, it doesn't hold the write lock: the
  // harness's writes on the calling thread go through at once, not after the purge's wait.
  it('leaves the write lock to other writes while it waits for a reader', async () => {
    const { writer, reader } = fixture()
    const wait = 800
    try {
      const probe = await writesDuring(runPurge({ path, root: 1, timeoutMs: wait }))
      expect(probe.error).toBeInstanceOf(WalBusyError)
      expect(probe.longest).toBeLessThan(200)
      expect(probe.writes).toBeGreaterThan(10)
    } finally {
      reader.close()
      writer.close()
    }
  })

  // A large conversation is deleted a chunk at a time: other writes go through in between.
  it('leaves the write lock to other writes while it deletes a large conversation', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const text = 'x'.repeat(400)
    writer.exec('BEGIN')
    const insert = writer.prepare('INSERT INTO entries (id, conversation_id, text) VALUES (?, 1, ?)')
    const record = writer.prepare('INSERT INTO record_ids (id) VALUES (?)')
    // Enough that deleting them in one transaction takes far longer than a write may wait here.
    for (let id = 1000; id < 251_000; id++) {
      insert.run(id, text)
      record.run(id)
    }
    writer.exec('COMMIT')
    writer.exec('PRAGMA wal_checkpoint(TRUNCATE)')
    try {
      const probe = await writesDuring(runPurge({ path, root: 1, timeoutMs: 5000 }))
      expect(probe.error).toBeUndefined()
      expect(probe.longest).toBeLessThan(200)
      expect(probe.writes).toBeGreaterThan(3)
      expect(writer.prepare('SELECT count(*) AS n FROM entries WHERE conversation_id = 1').get()).toEqual({ n: 0 })
      expect(writer.prepare('SELECT count(*) AS n FROM record_ids').get()).toEqual({ n: 3 }) // 2, 11 and 42
    } finally {
      reader.close()
      writer.close()
    }
  }, 30_000)

  // A purge that stops after some of its short transactions leaves what the next needs: the
  // conversations stay retired (their lifecycle documents are there), and the task that owns
  // conversation 3 still leads to it, so the retry removes conversation 3's records too.
  it('stops halfway and is finished by the next, which still finds every conversation', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    // Conversation 3, which a task of conversation 1 started, has more than one transaction's worth.
    writer.exec('BEGIN')
    const insert = writer.prepare('INSERT INTO entries (id, conversation_id, text) VALUES (?, 3, ?)')
    const record = writer.prepare('INSERT INTO record_ids (id) VALUES (?)')
    for (let id = 1000; id < 200_000; id++) {
      insert.run(id, WORD)
      record.run(id)
    }
    writer.exec('COMMIT')
    const stop = new Error('stopped')
    try {
      expect(() =>
        purgeRecords(path, 1, 1000, () => {
          throw stop
        }),
      ).toThrow(stop)
      const left = (q: string) => (writer.prepare(q).get() as { n: number }).n
      expect(left('SELECT count(*) AS n FROM entries WHERE conversation_id = 3')).toBeGreaterThan(0)
      expect(writer.prepare('SELECT id FROM conversations ORDER BY id').all()).toEqual([
        { id: 1 },
        { id: 2 },
        { id: 3 },
      ])
      expect(writer.prepare('SELECT id FROM tasks').all()).toEqual([{ id: 20 }])
      expect(writer.prepare(`SELECT id FROM documents WHERE kind = '${LIFECYCLE_KIND}' ORDER BY id`).all()).toEqual([
        { id: 43 },
        { id: 44 },
      ])
      expect(left('SELECT count(*) AS n FROM document_revisions WHERE document_id IN (43, 44)')).toBe(2)

      await runPurge({ path, root: 1, timeoutMs: 5000 })
      expect(writer.prepare('SELECT id FROM conversations').all()).toEqual([{ id: 2 }])
      expect(left('SELECT count(*) AS n FROM entries WHERE conversation_id <> 2')).toBe(0)
      expect(writer.prepare('SELECT id FROM tasks').all()).toEqual([])
      expect(writer.prepare('SELECT id FROM documents').all()).toEqual([{ id: 42 }])
      expect(writer.prepare('SELECT id FROM record_ids ORDER BY id').all()).toEqual([{ id: 2 }, { id: 11 }, { id: 42 }])
      expect(log()).not.toContain(WORD)
    } finally {
      reader.close()
      writer.close()
    }
  }, 30_000)

  // What a task writes while the purge pauses (an entry in the conversation it started, a new task
  // with a conversation of its own) goes with the rest: nothing of the tree outlives the purge.
  it('takes what was written during its pauses', () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    let wrote = false
    try {
      purgeRecords(path, 1, 1000, () => {
        if (wrote) return
        wrote = true
        writer.exec(`
          INSERT INTO entries (id, conversation_id, text) VALUES (50, 3, '${WORD}');
          INSERT INTO tasks (id, conversation_id) VALUES (21, 3);
          INSERT INTO conversations (id, owner_task_id) VALUES (5, 21);
          INSERT INTO entries (id, conversation_id, text) VALUES (51, 5, '${WORD}');
          INSERT INTO submissions (id, conversation_id) VALUES (52, 5);
          INSERT INTO record_ids (id) VALUES (50), (21), (5), (51), (52);
        `)
      })
      expect(wrote).toBe(true)
      expect(writer.prepare('SELECT id FROM conversations').all()).toEqual([{ id: 2 }])
      expect(writer.prepare('SELECT id FROM entries').all()).toEqual([{ id: 11 }])
      expect(writer.prepare('SELECT id FROM tasks').all()).toEqual([])
      expect(writer.prepare('SELECT id FROM submissions').all()).toEqual([])
      expect(writer.prepare('SELECT id FROM record_ids ORDER BY id').all()).toEqual([{ id: 2 }, { id: 11 }, { id: 42 }])
    } finally {
      reader.close()
      writer.close()
    }
  })

  it('rejects with the failure of a purge that is not about the log', async () => {
    await expect(runPurge({ path: join(dir, 'missing', 'agent.db'), root: 1, timeoutMs: 100 })).rejects.toThrow()
  })
})
