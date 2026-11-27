import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { availableParallelism, loadavg, tmpdir } from 'node:os'
import { join } from 'node:path'
import { DatabaseSync } from 'node:sqlite'
import { Worker } from 'node:worker_threads'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import {
  CHUNK,
  type FinalWork,
  framesLeft,
  HARNESS_DB_OPTIONS,
  LIFECYCLE_KIND,
  purgeOnce,
  purgeRecords,
  purgeWorkersAtOnce,
  purgeWorkersStarted,
  runPurge,
  SLICE_PAGES,
  SLICE_STEPS,
  type SliceTiming,
  startCheckpointer,
  storedKind,
  truncateWalWithin,
  WalBusyError,
} from './purge.js'

const WORD = 'doomed-marmalade-7731'

// What a timing test measured, printed whether it asserts or is skipped (on stderr, which the test
// runner shows for a test that passes or is skipped too).
function measured(line: string): void {
  process.stderr.write(`${line}\n`)
}

// A wall-clock bound means something only on a machine that isn't overloaded: when the load
// average (the higher of before and after the measurement) is above the number of cores, a timing
// test still measures and prints what it saw, and is skipped instead of asserting it. In CI it
// always asserts.
function overloaded(before: number): string | undefined {
  const load = Math.max(before, loadavg()[0])
  const cores = availableParallelism()
  if (process.env.CI || load <= cores) return undefined
  return `skipped: the load average is ${load.toFixed(1)} on ${cores} cores, so this machine can't tell a stall of the purge's from its own; the bound is asserted when the load is at most the cores, and always in CI`
}

// The most frames of the log the tests let one transaction of the purge leave for a checkpoint to
// copy, with no reader and no other writer: the pages its rows took (SLICE_PAGES at most, each
// written as zeros) and those SQLite dirtied besides, which the purge doesn't count (the leaves
// beside a freed one that it rebalanced, the indexes' and the marks' pages; measured: about as
// many again). A measured bound for these fixtures, not one the purge enforces.
const LOG_FRAMES = 3 * SLICE_PAGES

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
      CREATE TABLE tasks (id INTEGER PRIMARY KEY, conversation_id INTEGER, record TEXT);
      CREATE TABLE entries (id INTEGER PRIMARY KEY, conversation_id INTEGER, text TEXT);
      CREATE TABLE submissions (id INTEGER PRIMARY KEY, conversation_id INTEGER);
      CREATE TABLE documents (id INTEGER PRIMARY KEY, kind TEXT NOT NULL DEFAULT '${storedKind('notes')}', scope_kind TEXT, owner_id INTEGER);
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
      INSERT INTO documents (id, kind, scope_kind, owner_id) VALUES (43, '${storedKind(LIFECYCLE_KIND)}', 'conversation', 1),
        (44, '${storedKind(LIFECYCLE_KIND)}', 'conversation', 3);
      INSERT INTO document_revisions (document_id, seq, content) VALUES (43, 1, 'retired'), (44, 1, 'retired');
      INSERT INTO record_ids (id) VALUES (1), (2), (3), (10), (11), (20), (30), (40), (41), (42), (43), (44);
    `)
    writer.prepare('INSERT INTO entries (id, conversation_id, text) VALUES (10, 1, ?), (11, 2, ?)').run(WORD, 'kept')
    const reader = new DatabaseSync(path, { timeout: 100 })
    reader.exec('BEGIN')
    reader.prepare('SELECT count(*) FROM entries').get()
    return { writer, reader }
  }

  // Gives conversation 3 enough tasks that the sweep of the tree takes several short transactions,
  // so a test can write while the purge still holds its marks.
  const manyTasksUnder3 = (writer: DatabaseSync) => {
    const task = writer.prepare('INSERT INTO tasks (id, conversation_id) VALUES (?, 3)')
    writer.exec('BEGIN')
    for (let id = 300_000; id < 320_000; id++) task.run(id)
    writer.exec('COMMIT')
  }
  const marksLeft = (writer: DatabaseSync) =>
    (writer.prepare('SELECT count(*) AS n FROM taskto_purge_marks WHERE root = 1 AND id <> 1').get() as { n: number }).n

  // Folds a large setup into the database and has the disk write it out (fullfsync, for macOS), so
  // what a test measures is the purge, not the system writing back what the setup just made.
  const settle = (writer: DatabaseSync) => {
    writer.exec('PRAGMA fullfsync = ON')
    writer.exec('PRAGMA checkpoint_fullfsync = ON')
    writer.exec('PRAGMA wal_checkpoint(TRUNCATE)')
  }

  // A connection set up as Chord sets up the harness's with the sidecar's options.
  const harnessLike = (db: DatabaseSync) => {
    db.exec(`PRAGMA wal_autocheckpoint = ${HARNESS_DB_OPTIONS.walAutoCheckpointPages}`)
    db.exec('PRAGMA synchronous = NORMAL')
  }

  // The frames of the log no checkpoint has copied yet, as SQLite reports them without copying any
  // (NOOP, SQLite 3.51 and later: the Node CI and the image run).
  const pending = () => {
    const db = new DatabaseSync(path, { timeout: 0 })
    try {
      const row = db.prepare('PRAGMA wal_checkpoint(NOOP)').get() as { log: number; checkpointed: number }
      return Number(row.log) - Number(row.checkpointed)
    } finally {
      db.close()
    }
  }

  // The longest time, in ms, a connection that never waits couldn't take the write lock, trying every
  // 3 ms on a worker thread of its own until stop is called.
  const lockProbe = (path: string) => {
    const shared = new Int32Array(new SharedArrayBuffer(4))
    const worker = new Worker(
      `const { workerData, parentPort } = require('node:worker_threads')
      const { DatabaseSync } = require('node:sqlite')
      const db = new DatabaseSync(workerData.path, { timeout: 0 })
      let since = 0
      let longest = 0
      while (Atomics.load(workerData.stop, 0) === 0) {
        const now = performance.now()
        try {
          db.exec('BEGIN IMMEDIATE')
          db.exec('ROLLBACK')
          if (since) longest = Math.max(longest, now - since)
          since = 0
        } catch {
          if (!since) since = now
        }
        Atomics.wait(workerData.stop, 0, 0, 3)
      }
      if (since) longest = Math.max(longest, performance.now() - since)
      db.close()
      parentPort.postMessage(longest)`,
      { eval: true, workerData: { path, stop: shared } },
    )
    const result = new Promise<number>((resolve, reject) => {
      worker.once('message', resolve)
      worker.once('error', reject)
    })
    return {
      stop: () => {
        Atomics.store(shared, 0, 1)
        Atomics.notify(shared, 0)
        return result
      },
    }
  }

  // Writes on the calling thread, as the harness's commits are, every 15 ms while work is pending,
  // each on a connection that waits up to 5 s for the write lock: how long the longest one took,
  // and the longest gap between two ticks of a 10 ms timer there (what a health check would wait).
  // With lockProbe, also the longest time the write lock was held by someone else: on a thread of its
  // own (so a write that waits here doesn't stop it), a connection that never waits tries to take it
  // every 3 ms, and the longest run of refusals is how long it couldn't. That tells a stall the
  // purge's lock made from one the disk made (the write's own I/O).
  const writesDuring = async (work: Promise<unknown>, options: { lockProbe?: boolean } = {}) => {
    const db = new DatabaseSync(path, { timeout: 5000 })
    // Configured as the sidecar configures the harness's connection (HARNESS_DB_OPTIONS, and
    // synchronous = NORMAL as Chord sets it).
    harnessLike(db)
    const insert = db.prepare("INSERT INTO entries (conversation_id, text) VALUES (2, 'another turn')")
    let longest = 0
    let writes = 0
    let done = false
    let last = performance.now()
    let gap = 0
    const timer = setInterval(() => {
      const now = performance.now()
      gap = Math.max(gap, now - last)
      last = now
    }, 10)
    const probe = options.lockProbe ? lockProbe(path) : undefined
    let report: unknown
    const settled = work.then(
      (value) => {
        report = value
        return undefined
      },
      (error: unknown) => error,
    )
    void settled.then(() => {
      done = true
    })
    const began = performance.now()
    while (!done) {
      const t0 = performance.now()
      insert.run()
      longest = Math.max(longest, performance.now() - t0)
      writes++
      await new Promise((r) => setTimeout(r, 15))
    }
    const took = performance.now() - began
    clearInterval(timer)
    const locked = probe ? await probe.stop() : 0
    db.close()
    const error = await settled
    // What the purge's own transactions took (lock, deleting, commit), and how long the write lock
    // was held as the probe saw it, to say which a stall was.
    return { longest, gap, writes, took, error, locked, report: JSON.stringify({ lockHeld: locked, purge: report }) }
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
      // A checkpoint that waits holding the lock makes a write wait for nearly all of it.
      expect(probe.longest).toBeLessThan(wait / 4)
      expect(probe.writes).toBeGreaterThan(10)
    } finally {
      reader.close()
      writer.close()
    }
  })

  // A large conversation (about 200 MiB) is deleted a chunk at a time: other writes go through in
  // between, and the calling thread keeps answering.
  it('leaves the write lock to other writes while it deletes a large conversation', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const text = 'x'.repeat(800)
    writer.exec('BEGIN')
    const insert = writer.prepare('INSERT INTO entries (id, conversation_id, text) VALUES (?, 1, ?)')
    const record = writer.prepare('INSERT INTO record_ids (id) VALUES (?)')
    // Enough that deleting them in one transaction takes far longer than a write may wait here.
    for (let id = 1000; id < 251_000; id++) {
      insert.run(id, text)
      record.run(id)
    }
    writer.exec('COMMIT')
    settle(writer)
    try {
      const probe = await writesDuring(runPurge({ path, root: 1, timeoutMs: 5000 }))
      expect(probe.error).toBeUndefined()
      // Measured against the purge itself, which a slower disk makes longer: in one transaction
      // a write waited for nearly all of it (about 1 s of 1.1 s here), in short ones for a few
      // hundredths (36 ms of 2.6 s).
      expect(probe.longest).toBeLessThan(probe.took / 4)
      expect(probe.gap).toBeLessThan(probe.took / 4)
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
        purgeRecords(path, 1, 1000, {
          afterSlice: () => {
            throw stop
          },
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
      // The root is still retired (its lifecycle document goes last), and what is left of the tree
      // is reachable from it or marked.
      expect(
        writer.prepare(`SELECT id FROM documents WHERE kind = '${storedKind(LIFECYCLE_KIND)}' AND owner_id = 1`).all(),
      ).toEqual([{ id: 43 }])
      expect(left('SELECT count(*) AS n FROM document_revisions WHERE document_id = 43')).toBe(1)
      expect(
        left("SELECT count(*) AS n FROM taskto_purge_marks WHERE root = 1 AND kind = 'conversation' AND id = 3"),
      ).toBe(1)

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
    manyTasksUnder3(writer)
    let wrote = false
    try {
      purgeRecords(path, 1, 1000, {
        afterSlice: () => {
          // While the purge holds its marks (retiring stops all the tree's work, so in the sidecar
          // nothing writes after them): conversation 3's row may be gone already.
          if (wrote || marksLeft(writer) === 0) return
          wrote = true
          writer.exec(`
          INSERT INTO entries (id, conversation_id, text) VALUES (50, 3, '${WORD}');
          INSERT INTO tasks (id, conversation_id) VALUES (21, 3);
          INSERT INTO conversations (id, owner_task_id) VALUES (5, 21);
          INSERT INTO entries (id, conversation_id, text) VALUES (51, 5, '${WORD}');
          INSERT INTO submissions (id, conversation_id) VALUES (52, 5);
          INSERT INTO record_ids (id) VALUES (50), (21), (5), (51), (52);
        `)
        },
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

  // A revision committed to a document after the revisions' own step, and before the document's
  // row goes, goes with the document: nothing else could find it once the document is gone.
  it('takes a revision written between the revisions and their document', () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    // Enough revisions, and documents, that each step takes several short transactions; the
    // document written to is the last of its step.
    const doc = 900_000
    writer.exec('BEGIN')
    writer.prepare("INSERT INTO documents (id, scope_kind, owner_id) VALUES (?, 'conversation', 1)").run(doc)
    const revision = writer.prepare('INSERT INTO document_revisions (document_id, seq, content) VALUES (?, ?, ?)')
    for (let seq = 1; seq <= 30_000; seq++) revision.run(doc, seq, 'an old version')
    const other = writer.prepare("INSERT INTO documents (id, scope_kind, owner_id) VALUES (?, 'conversation', 1)")
    for (let id = 100_000; id < 130_000; id++) other.run(id)
    writer.exec('COMMIT')
    const docLeft = writer.prepare('SELECT count(*) AS n FROM documents WHERE id = ?')
    const oldLeft = writer.prepare(
      'SELECT count(*) AS n FROM document_revisions WHERE document_id = ? AND seq <= 30000',
    )
    const othersLeft = writer.prepare('SELECT count(*) AS n FROM documents WHERE id BETWEEN 100000 AND 129999')
    let wrote = false
    try {
      // The reviewer's sequence: the revisions' step is done, a first batch of documents has gone,
      // and a revision is written for a document still there, whose batch comes later.
      purgeRecords(path, 1, 1000, {
        afterSlice: () => {
          const hasDoc = (docLeft.get(doc) as { n: number }).n > 0
          const oldGone = (oldLeft.get(doc) as { n: number }).n === 0
          const batchGone = (othersLeft.get() as { n: number }).n < 30_000
          if (wrote || !hasDoc || !oldGone || !batchGone) return
          wrote = true
          revision.run(doc, 30_001, WORD)
        },
      })
      expect(wrote).toBe(true)
      expect(writer.prepare('SELECT count(*) AS n FROM document_revisions WHERE document_id = ?').get(doc)).toEqual({
        n: 0,
      })
      expect(writer.prepare('SELECT document_id FROM document_revisions').all()).toEqual([{ document_id: 42 }])
    } finally {
      reader.close()
      writer.close()
    }
  }, 30_000)

  // A long task history (60,000 finished tasks of about 4 KB) goes in short transactions too: the
  // last one, which the harness's writes would wait for, only checks again and takes the root.
  it('leaves the write lock to other writes while it deletes a long task history', async (ctx) => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const record = writer.prepare('INSERT INTO record_ids (id) VALUES (?)')
    const task = writer.prepare('INSERT INTO tasks (id, conversation_id, record) VALUES (?, 1, ?)')
    const big = 'r'.repeat(4000)
    writer.exec('BEGIN')
    for (let id = 1_000_000; id < 1_060_000; id++) {
      task.run(id, big)
      record.run(id)
    }
    writer.exec('COMMIT')
    settle(writer)
    try {
      const load = loadavg()[0]
      const probe = await writesDuring(runPurge({ path, root: 1, timeoutMs: 10_000, report: true }), {
        lockProbe: true,
      })
      expect(probe.error).toBeUndefined()
      expect(writer.prepare('SELECT count(*) AS n FROM tasks').get()).toEqual({ n: 0 })
      expect(writer.prepare('SELECT id FROM conversations').all()).toEqual([{ id: 2 }])
      measured(
        `long task history: longest write ${probe.longest.toFixed(1)} ms, timer gap ${probe.gap.toFixed(1)} ms; ${probe.report}`,
      )
      const skip = overloaded(load)
      ctx.skip(skip !== undefined, skip && `${skip} (${probe.longest.toFixed(1)} ms measured)`)
      // A fixed bound on the longest stall, whatever the history's length: no transaction of the
      // purge grows with it (in one last transaction a write waited for nearly all of it).
      expect(probe.longest, probe.report).toBeLessThan(250)
      expect(probe.gap, probe.report).toBeLessThan(250)
    } finally {
      reader.close()
      writer.close()
    }
  }, 60_000)

  // A purge that stops while it takes the tree apart leaves what is left reachable from the root:
  // a task goes only once no conversation it started is left, a conversation once no task of it is.
  it('stops while it takes the tree apart and is finished by the next', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    // Conversation 3 (started by task 20 of the root) has many tasks, one of which started
    // conversation 6, with tasks of its own.
    const task = writer.prepare('INSERT INTO tasks (id, conversation_id) VALUES (?, ?)')
    writer.exec('BEGIN')
    for (let id = 100_000; id < 120_000; id++) task.run(id, 3)
    writer.exec('INSERT INTO conversations (id, owner_task_id) VALUES (6, 100000)')
    for (let id = 200_000; id < 220_000; id++) task.run(id, 6)
    writer.exec(`INSERT INTO entries (id, conversation_id, text) VALUES (60, 6, '${WORD}')`)
    writer.exec('COMMIT')
    const tasksLeft = () => (writer.prepare('SELECT count(*) AS n FROM tasks').get() as { n: number }).n
    const stop = new Error('stopped')
    try {
      expect(() =>
        purgeRecords(path, 1, 1000, {
          afterSlice: () => {
            if (tasksLeft() < 30_000) throw stop
          },
        }),
      ).toThrow(stop)
      // Partway through the tree: some tasks are gone, and every one left is marked or its
      // conversation is still there, so the next purge finds it from the marks.
      expect(tasksLeft()).toBeGreaterThan(0)
      expect(
        writer
          .prepare(`SELECT count(*) AS n FROM tasks t WHERE NOT EXISTS
            (SELECT 1 FROM taskto_purge_marks m WHERE m.root = 1 AND m.kind = 'task' AND m.id = t.id)
            AND NOT EXISTS (SELECT 1 FROM conversations c WHERE c.id = t.conversation_id)`)
          .get(),
      ).toEqual({ n: 0 })
      await runPurge({ path, root: 1, timeoutMs: 5000 })
      expect(tasksLeft()).toBe(0)
      expect(writer.prepare('SELECT id FROM conversations').all()).toEqual([{ id: 2 }])
      expect(writer.prepare('SELECT id FROM entries').all()).toEqual([{ id: 11 }])
    } finally {
      reader.close()
      writer.close()
    }
  }, 60_000)

  // A reader that held an old snapshot while a large backlog was written lets go just after a round
  // found the backlog: the log is emptied (by the TRUNCATE checkpoint, which holds the write lock
  // for as long as it copies) only in a round that found it drained, after a passive checkpoint,
  // which writers go on beside, copied the backlog.
  // A passive checkpoint that copied a large backlog took long, and what was committed meanwhile is
  // past where it looked: the TRUNCATE doesn't follow it, but a round whose checkpoint was quick.
  it('truncates only after a quick passive checkpoint found the log drained', () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const more = writer.prepare('INSERT INTO entries (conversation_id, text) VALUES (2, ?)')
    writer.exec('BEGIN')
    for (let i = 0; i < 300_000; i++) more.run('m'.repeat(400))
    writer.exec('COMMIT')
    const db = new DatabaseSync(path, { timeout: 1000 })
    const rounds: { backlog: number; ms: number }[] = []
    let truncatedAfter: { backlog: number; ms: number } | undefined
    try {
      truncateWalWithin(db, 20_000, {
        round: (backlog, ms) => rounds.push({ backlog, ms }),
        truncating: () => (truncatedAfter = rounds[rounds.length - 1]),
      })
      expect(rounds[0].ms).toBeGreaterThan(20) // the backlog took long to copy
      expect(truncatedAfter).toBeDefined()
      expect(truncatedAfter?.ms).toBeLessThanOrEqual(20)
      expect(rounds.length).toBeGreaterThan(1)
      expect(log()).toBe('')
    } finally {
      db.close()
      reader.close()
      writer.close()
    }
  })

  // The server's next try of a purge (it retires the conversation first, then purges) can write a
  // lifecycle document for a conversation the last purge already took, when the runtime still has
  // it in memory: the purge takes that document too.
  it('takes a lifecycle document written again for a purged conversation', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    try {
      await runPurge({ path, root: 1, timeoutMs: 5000 })
      writer.exec(`
        INSERT INTO documents (id, kind, scope_kind, owner_id) VALUES (90, '${storedKind(LIFECYCLE_KIND)}', 'conversation', 1);
        INSERT INTO document_revisions (document_id, seq, content) VALUES (90, 1, 'retired again');
        INSERT INTO record_ids (id) VALUES (90);
      `)
      await runPurge({ path, root: 1, timeoutMs: 5000 })
      expect(writer.prepare('SELECT id FROM documents WHERE id = 90').all()).toEqual([])
      expect(writer.prepare('SELECT count(*) AS n FROM document_revisions WHERE document_id = 90').get()).toEqual({
        n: 0,
      })
    } finally {
      reader.close()
      writer.close()
    }
  })

  it('empties the log only once a passive checkpoint has drained it', () => {
    const { writer, reader } = fixture() // the reader holds its snapshot from before the backlog
    const text = 'y'.repeat(400)
    const more = writer.prepare('INSERT INTO entries (conversation_id, text) VALUES (2, ?)')
    writer.exec('BEGIN')
    for (let i = 0; i < 100_000; i++) more.run(text)
    writer.exec('COMMIT')
    const db = new DatabaseSync(path, { timeout: 1000 })
    const rounds: number[] = []
    try {
      truncateWalWithin(db, 10_000, {
        round: (backlog) => {
          rounds.push(backlog)
          if (rounds.length === 1) reader.exec('COMMIT') // lets go between the round's two checkpoints
        },
      })
      expect(rounds[0]).toBeGreaterThan(1000) // the reader kept the backlog from being copied
      // The round that emptied the log is the last one: it found the backlog copied.
      expect(rounds.length).toBeGreaterThan(1)
      expect(rounds[rounds.length - 1]).toBeLessThanOrEqual(64)
      expect(log()).toBe('')
    } finally {
      db.close()
      reader.close()
      writer.close()
    }
  })

  // A commit that lands after the round's passive checkpoint found the log drained is copied by a
  // second look just before the TRUNCATE, not by the TRUNCATE under the write lock.
  it('looks at the log again just before the TRUNCATE', () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const more = writer.prepare('INSERT INTO entries (conversation_id, text) VALUES (2, ?)')
    const db = new DatabaseSync(path, { timeout: 1000 })
    let committed = false
    const truncated: number[] = []
    try {
      truncateWalWithin(db, 10_000, {
        round: (backlog) => {
          if (committed || backlog > 64) return
          committed = true
          writer.exec('BEGIN')
          for (let i = 0; i < 10_000; i++) more.run('k'.repeat(400))
          writer.exec('COMMIT')
        },
        truncating: () => truncated.push(pending()),
      })
      expect(committed).toBe(true)
      expect(truncated.length).toBe(1)
      expect(truncated[0]).toBeLessThanOrEqual(64)
      expect(log()).toBe('')
    } finally {
      db.close()
      reader.close()
      writer.close()
    }
  })

  // The bound the TRUNCATE keeps (truncateWalWithin): under the write lock it copies what the round's
  // passive checkpoint left (64 frames at most) and what was committed between the two, no more. A
  // commit that lands in that moment (the harness's, which nothing gates) is copied with it, and the
  // log is still emptied.
  it('copies under the lock only what the passive checkpoint left and what landed since', () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const text = 'z'.repeat(400)
    const more = writer.prepare('INSERT INTO entries (conversation_id, text) VALUES (2, ?)')
    const db = new DatabaseSync(path, { timeout: 1000 })
    const seen: { backlog: number; before: number; after: number }[] = []
    try {
      truncateWalWithin(db, 10_000, {
        truncating: (backlog) => {
          const before = pending()
          if (seen.length === 0) {
            writer.exec('BEGIN')
            for (let i = 0; i < 20_000; i++) more.run(text)
            writer.exec('COMMIT')
          }
          seen.push({ backlog, before, after: pending() })
        },
      })
      expect(seen.length).toBeGreaterThan(0)
      // The round found the log drained, and nothing else came before the commit in the window.
      expect(seen[0].backlog).toBeLessThanOrEqual(64)
      expect(seen[0].before).toBeLessThanOrEqual(64)
      // What the TRUNCATE then copied: that, and the commit.
      expect(seen[0].after).toBeGreaterThan(seen[0].before)
      expect(log()).toBe('')
    } finally {
      db.close()
      reader.close()
      writer.close()
    }
  })

  it('leaves the log to the checkpointer when a commit follows a busy purge', async () => {
    const { writer, reader } = fixture()
    const text = 'w'.repeat(800)
    const insert = writer.prepare('INSERT INTO entries (id, conversation_id, text) VALUES (?, 1, ?)')
    const record = writer.prepare('INSERT INTO record_ids (id) VALUES (?)')
    writer.exec('BEGIN')
    for (let id = 1000; id < 51_000; id++) {
      insert.run(id, text)
      record.run(id)
    }
    writer.exec('COMMIT')
    const harness = new DatabaseSync(path, { timeout: 5000 })
    harnessLike(harness)
    try {
      await expect(runPurge({ path, root: 1, timeoutMs: 300 })).rejects.toBeInstanceOf(WalBusyError)
      reader.exec('COMMIT')
      const before = pending()
      expect(before).toBeGreaterThan(1000)
      harness.prepare("INSERT INTO entries (conversation_id, text) VALUES (2, 'an unrelated turn')").run()
      expect(pending()).toBeGreaterThanOrEqual(before)
    } finally {
      harness.close()
      reader.close()
      writer.close()
    }
  }, 60_000)

  // The checkpointer copies the log on its own thread: with the commits' own checkpoints off, the
  // log a large write left is copied all the same.
  it('copies the log on a thread of its own when the commits do not', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    harnessLike(writer)
    const more = writer.prepare('INSERT INTO entries (conversation_id, text) VALUES (2, ?)')
    writer.exec('BEGIN')
    for (let i = 0; i < 20_000; i++) more.run('v'.repeat(400))
    writer.exec('COMMIT')
    expect(pending()).toBeGreaterThan(1000)
    const stop = startCheckpointer(path, 20)
    try {
      const until = Date.now() + 5000
      while (pending() > 0 && Date.now() < until) await new Promise((r) => setTimeout(r, 20))
      expect(pending()).toBe(0)
    } finally {
      await stop()
      reader.close()
      writer.close()
    }
  })

  // A record committed for a child conversation after its row went, and then the purge stops: the
  // retry still finds it, from the tree the purge remembered before removing anything.
  it('finds a late record of a removed child after a stop', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    manyTasksUnder3(writer)
    const childGone = () =>
      (writer.prepare('SELECT count(*) AS n FROM conversations WHERE id = 3').get() as { n: number }).n === 0
    const stop = new Error('stopped')
    try {
      expect(() =>
        purgeRecords(path, 1, 1000, {
          afterSlice: () => {
            if (!childGone() || marksLeft(writer) === 0) return
            // A write for the child that was already in flight lands now, and then the purge stops.
            writer.exec(`INSERT INTO entries (id, conversation_id, text) VALUES (70, 3, '${WORD}')`)
            writer.exec('INSERT INTO record_ids (id) VALUES (70)')
            throw stop
          },
        }),
      ).toThrow(stop)
      expect(writer.prepare('SELECT id FROM entries WHERE id = 70').all()).toEqual([{ id: 70 }])
      await runPurge({ path, root: 1, timeoutMs: 5000 })
      expect(writer.prepare('SELECT id FROM entries').all()).toEqual([{ id: 11 }])
      expect(writer.prepare('SELECT id FROM record_ids ORDER BY id').all()).toEqual([{ id: 2 }, { id: 11 }, { id: 42 }])
      expect(writer.prepare("SELECT count(*) AS n FROM sqlite_master WHERE name = 'taskto_purge_marks'").get()).toEqual(
        {
          n: 1,
        },
      )
      expect(writer.prepare('SELECT count(*) AS n FROM taskto_purge_marks').get()).toEqual({ n: 0 })
    } finally {
      reader.close()
      writer.close()
    }
  })

  // Purges root 1 of a fresh database that fill gave what is to go, and says what each transaction
  // did: its work (SliceTiming) and the frames of the log it left for a checkpoint to copy (read
  // before the purge's own checkpoint after it), and the same for the last one.
  const workOf = (name: string, fill: (writer: DatabaseSync) => void) => {
    path = join(dir, `${name}.db`)
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    writer.exec('BEGIN')
    fill(writer)
    writer.exec('COMMIT')
    writer.exec('PRAGMA wal_checkpoint(TRUNCATE)')
    const slices: SliceTiming[] = []
    const frames: number[] = []
    let final: FinalWork | undefined
    let finalFrames = 0
    try {
      purgeRecords(path, 1, 10_000, {
        slice: (_, t) => slices.push(t),
        afterSlice: () => frames.push(pending()),
        final: (_, work) => {
          final = work
          finalFrames = pending()
        },
      })
      expect(writer.prepare('SELECT count(*) AS n FROM tasks').get()).toEqual({ n: 0 })
      expect(writer.prepare('SELECT count(*) AS n FROM entries WHERE conversation_id <> 2').get()).toEqual({ n: 0 })
      expect(writer.prepare('SELECT document_id FROM document_revisions').all()).toEqual([{ document_id: 42 }])
    } finally {
      reader.close()
      writer.close()
    }
    const most = (key: keyof SliceTiming) => Math.max(...slices.map((s) => s[key]))
    const seen = `${name}: ${JSON.stringify({ slices: slices.length, pages: most('pages'), records: most('records'), steps: most('steps'), changes: most('changes'), frames: Math.max(...frames), final, finalFrames })}`
    return { slices, frames, final, finalFrames, most, seen }
  }

  // The work of every transaction of the purge, counted, not timed: a step takes CHUNK records at
  // most, a transaction SLICE_STEPS steps and SLICE_PAGES freed pages at most (with records of 4 KB,
  // about a page each, and of 100 KB, 25 pages each), and the last transaction does the same work
  // however large the account. What each commits is copied into the database before the next
  // begins, so the log never holds more than one transaction's frames for a checkpoint to copy.
  it('bounds the work of every transaction by rows and pages, whatever the account', () => {
    const run = (tasks: number, size: number) =>
      workOf(`bounds-${tasks}-${size}`, (writer) => {
        const task = writer.prepare('INSERT INTO tasks (id, conversation_id, record) VALUES (?, ?, ?)')
        const record = writer.prepare('INSERT INTO record_ids (id) VALUES (?)')
        const entry = writer.prepare('INSERT INTO entries (id, conversation_id, text) VALUES (?, 3, ?)')
        const body = 'r'.repeat(size)
        for (let id = 1_000_000; id < 1_000_000 + tasks; id++) {
          task.run(id, id % 2 ? 1 : 3, body)
          record.run(id)
        }
        // And what was said in the child conversation, in records of the same size.
        for (let id = 2_000_000; id < 2_000_000 + tasks / 4; id++) {
          entry.run(id, body)
          record.run(id)
        }
      })
    const runs = { small: run(2_000, 4000), large: run(20_000, 4000), wide: run(400, 100_000) }
    for (const r of Object.values(runs)) {
      const { most, seen } = r
      expect(most('pages'), seen).toBeLessThanOrEqual(SLICE_PAGES)
      // The log is copied as the purge goes: no transaction finds more than its own frames there.
      expect(Math.max(...r.frames), seen).toBeLessThanOrEqual(LOG_FRAMES)
      expect(r.finalFrames, seen).toBeLessThanOrEqual(LOG_FRAMES)
      expect(most('records'), seen).toBeLessThanOrEqual(CHUNK)
      expect(most('steps'), seen).toBeLessThanOrEqual(SLICE_STEPS)
      // A record's row, its record_ids row and its mark.
      expect(most('changes'), seen).toBeLessThanOrEqual(3 * CHUNK)
    }
    // The last transaction: the root's row, its lifecycle document with its revision, their
    // record_ids rows and the root's mark, however many tasks went before it.
    expect(runs.large.final).toEqual(runs.small.final)
    expect(runs.large.final?.changes).toBeLessThanOrEqual(8)
    expect(runs.large.final?.pages).toBeLessThanOrEqual(4)
    expect(runs.large.slices.length).toBeGreaterThan(runs.small.slices.length * 5)
  }, 120_000)

  // The pages of a delete are worked out from the sizes of the rows it is about to take, so a table
  // whose records change size from one to the next (small ones, then a run of wide ones of 256 KB,
  // 64 pages each, or the other way) stays within SLICE_PAGES: not from what earlier rows freed.
  // Tasks, what was said, and a document's revisions, in both orders.
  it('sizes each delete by the rows it takes when records change size', () => {
    const small = 'r'.repeat(200)
    const wide = 'w'.repeat(256 * 1024)
    // Small records first in the order the purge reads them, or wide ones first (the low ids are
    // read first by the sweep's tasks, the high ids by entries, and revisions by their key).
    const sizes = (wideFirst: boolean) => Array.from({ length: 140 }, (_, i) => (i < 100 !== wideFirst ? small : wide))
    const kinds = {
      tasks: (writer: DatabaseSync, bodies: string[]) => {
        const task = writer.prepare('INSERT INTO tasks (id, conversation_id, record) VALUES (?, 1, ?)')
        for (const [i, b] of bodies.entries()) task.run(1_000_000 + i, b)
      },
      entries: (writer: DatabaseSync, bodies: string[]) => {
        const entry = writer.prepare('INSERT INTO entries (id, conversation_id, text) VALUES (?, 1, ?)')
        for (const [i, b] of bodies.entries()) entry.run(2_000_000 - i, b)
      },
      revisions: (writer: DatabaseSync, bodies: string[]) => {
        writer.exec("INSERT INTO documents (id, scope_kind, owner_id) VALUES (3_000_000, 'conversation', 1)")
        const revision = writer.prepare(
          'INSERT INTO document_revisions (document_id, seq, content) VALUES (3_000_000, ?, ?)',
        )
        for (const [i, b] of bodies.entries()) revision.run(i + 1, b)
      },
    }
    for (const [kind, fill] of Object.entries(kinds)) {
      for (const wideFirst of [false, true]) {
        const r = workOf(`sizes-${kind}-${wideFirst ? 'wide' : 'small'}-first`, (writer) =>
          fill(writer, sizes(wideFirst)),
        )
        expect.soft(r.most('pages'), r.seen).toBeLessThanOrEqual(SLICE_PAGES)
        expect.soft(Math.max(...r.frames), r.seen).toBeLessThanOrEqual(LOG_FRAMES)
      }
    }
  }, 120_000)

  // A record larger than SLICE_PAGES goes alone, in a transaction of its own, and the purge goes on.
  it('takes a record larger than a transaction alone', () => {
    const huge = 'h'.repeat(4 * 1024 * 1024) // about 1,000 pages
    const r = workOf('huge', (writer) => {
      const entry = writer.prepare('INSERT INTO entries (id, conversation_id, text) VALUES (?, 1, ?)')
      for (let i = 0; i < 10; i++) entry.run(2_000_000 + i, i === 5 ? huge : 'small')
    })
    const big = r.slices.filter((s) => s.pages > SLICE_PAGES)
    expect(big.length, r.seen).toBe(1)
    expect(big[0].records, r.seen).toBe(1)
  }, 120_000)

  // The purge's last transaction checks again from the root and what is still marked, and takes
  // the root: it doesn't grow with the tree (60,000 tasks of about 4 KB here, swept before it).
  it('ends with a short transaction however long the task history', () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const task = writer.prepare('INSERT INTO tasks (id, conversation_id, record) VALUES (?, 1, ?)')
    const record = writer.prepare('INSERT INTO record_ids (id) VALUES (?)')
    const big = 'r'.repeat(4000)
    writer.exec('BEGIN')
    for (let id = 1_000_000; id < 1_060_000; id++) {
      task.run(id, big)
      record.run(id)
    }
    writer.exec('COMMIT')
    let last = Number.POSITIVE_INFINITY
    try {
      purgeRecords(path, 1, 10_000, { final: (ms) => (last = ms) })
      expect(last).toBeLessThan(25)
      expect(writer.prepare('SELECT count(*) AS n FROM tasks').get()).toEqual({ n: 0 })
      expect(writer.prepare('SELECT count(*) AS n FROM taskto_purge_marks').get()).toEqual({ n: 0 })
    } finally {
      reader.close()
      writer.close()
    }
  }, 60_000)

  // A task made after the purge found the tree (under the root, and under a child whose row the
  // purge may have taken already), and later a document of that task with a revision: both go,
  // whether the purge runs to the end or stops after the document was written and is tried again.
  for (const stops of [false, true]) {
    it(`takes the records of a task made after the tree was found${stops ? ', across a stop' : ''}`, async () => {
      const { writer, reader } = fixture()
      reader.exec('COMMIT')
      manyTasksUnder3(writer)
      let slice = 0
      const stop = new Error('stopped')
      const run = () =>
        purgeRecords(path, 1, 1000, {
          afterSlice: () => {
            slice++
            if (slice === 1) {
              writer.exec(`
                INSERT INTO tasks (id, conversation_id) VALUES (50000, 1), (50002, 3);
                INSERT INTO record_ids (id) VALUES (50000), (50002);
              `)
            }
            if (slice === 3) {
              writer.exec(`
                INSERT INTO documents (id, scope_kind, owner_id) VALUES (50001, 'task', 50000), (50003, 'task', 50002);
                INSERT INTO document_revisions (document_id, seq, content) VALUES (50001, 1, '${WORD}'), (50003, 1, '${WORD}');
                INSERT INTO record_ids (id) VALUES (50001), (50003);
              `)
              if (stops) throw stop
            }
          },
        })
      try {
        if (stops) {
          expect(run).toThrow(stop)
          await runPurge({ path, root: 1, timeoutMs: 5000 })
        } else {
          run()
        }
        expect(slice).toBeGreaterThanOrEqual(3)
        expect(writer.prepare('SELECT id FROM tasks WHERE id IN (50000, 50002)').all()).toEqual([])
        expect(writer.prepare('SELECT id FROM documents WHERE id IN (50001, 50003)').all()).toEqual([])
        expect(writer.prepare('SELECT document_id FROM document_revisions').all()).toEqual([{ document_id: 42 }])
        expect(writer.prepare('SELECT id FROM record_ids ORDER BY id').all()).toEqual([
          { id: 2 },
          { id: 11 },
          { id: 42 },
        ])
      } finally {
        reader.close()
        writer.close()
      }
    }, 60_000)
  }

  // SQLite answers a checkpoint that another connection's checkpoint keeps out with busy = 1 and
  // log -1, not an error; only busy = 0 with log -1 means the database isn't in WAL mode.
  it('reads a busy checkpoint as not drained', () => {
    expect(framesLeft({ busy: 1, log: -1, checkpointed: -1 })).toBe(Number.POSITIVE_INFINITY)
    expect(framesLeft({ busy: 0, log: -1, checkpointed: -1 })).toBe(0)
    expect(framesLeft({ busy: 0, log: 10, checkpointed: 4 })).toBe(6)
    expect(framesLeft({ busy: 1, log: 10, checkpointed: 4 })).toBe(Number.POSITIVE_INFINITY)
  })

  // Another thread's checkpoint (the checkpointer's, or another purge's) is copying a large log when
  // the purge looks: that round learned nothing, and the TRUNCATE waits for a round whose own
  // passive checkpoint found the log drained.
  it('does not truncate while another checkpoint is copying the log', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const more = writer.prepare('INSERT INTO entries (conversation_id, text) VALUES (2, ?)')
    writer.exec('BEGIN')
    for (let i = 0; i < 300_000; i++) more.run('q'.repeat(400))
    writer.exec('COMMIT')
    const flag = new Int32Array(new SharedArrayBuffer(4))
    const other = new Worker(
      `const { DatabaseSync } = require('node:sqlite')
       const { workerData } = require('node:worker_threads')
       const db = new DatabaseSync(workerData.path, { timeout: 0 })
       Atomics.store(workerData.flag, 0, 1)
       Atomics.notify(workerData.flag, 0)
       db.prepare('PRAGMA wal_checkpoint(PASSIVE)').get()
       db.close()`,
      { eval: true, workerData: { path, flag } },
    )
    const ended = new Promise((resolve) => other.once('exit', resolve))
    Atomics.wait(flag, 0, 0, 5000)
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 5) // it is copying now
    const db = new DatabaseSync(path, { timeout: 1000 })
    const rounds: number[] = []
    const truncated: number[] = []
    try {
      truncateWalWithin(db, 20_000, { round: (b) => rounds.push(b), truncating: () => truncated.push(pending()) })
      expect(rounds[0]).toBe(Number.POSITIVE_INFINITY)
      for (const p of truncated) expect(p).toBeLessThanOrEqual(64)
      expect(log()).toBe('')
    } finally {
      await ended
      db.close()
      reader.close()
      writer.close()
    }
  }, 60_000)

  // A very large account (150,000 tasks): each of the purge's transactions does a bounded amount of
  // work (the marks are read from their index, the tree is marked a batch at a time, never by one
  // list of every id), the harness's writes wait a fixed short time at most, and a purge that
  // stopped carries on from its marks without going over the whole tree again.
  it('keeps every transaction short on a very large account, and carries on after a stop', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const task = writer.prepare("INSERT INTO tasks (id, conversation_id, record) VALUES (?, 1, '{}')")
    const record = writer.prepare('INSERT INTO record_ids (id) VALUES (?)')
    writer.exec('BEGIN')
    for (let id = 1_000_000; id < 1_150_000; id++) {
      task.run(id)
      record.run(id)
    }
    writer.exec('COMMIT')
    settle(writer)
    const stop = new Error('stopped')
    const slices: number[] = []
    let started = performance.now()
    let firstSlice = Number.POSITIVE_INFINITY
    let count = 0
    try {
      expect(() =>
        purgeRecords(path, 1, 10_000, {
          slice: (ms) => slices.push(ms),
          afterSlice: () => {
            if (++count === 20) throw stop
          },
        }),
      ).toThrow(stop)
      // The retry: its first transaction comes at once, not after a look at the whole tree.
      started = performance.now()
      purgeRecords(path, 1, 10_000, {
        slice: (ms) => slices.push(ms),
        afterSlice: () => {
          if (firstSlice === Number.POSITIVE_INFINITY) firstSlice = performance.now() - started
        },
      })
      const took = performance.now() - started
      expect(firstSlice).toBeLessThan(200)
      expect(Math.max(...slices)).toBeLessThan(100)
      expect(took).toBeLessThan(20_000)
      expect(writer.prepare('SELECT count(*) AS n FROM tasks').get()).toEqual({ n: 0 })
      expect(writer.prepare('SELECT count(*) AS n FROM taskto_purge_marks').get()).toEqual({ n: 0 })
    } finally {
      reader.close()
      writer.close()
    }
  }, 120_000)

  // The same on its thread, with the harness's writes beside it: a fixed bound on the longest stall.
  it('leaves the write lock to other writes on a very large account', async (ctx) => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const task = writer.prepare("INSERT INTO tasks (id, conversation_id, record) VALUES (?, 1, '{}')")
    const record = writer.prepare('INSERT INTO record_ids (id) VALUES (?)')
    writer.exec('BEGIN')
    for (let id = 1_000_000; id < 1_150_000; id++) {
      task.run(id)
      record.run(id)
    }
    writer.exec('COMMIT')
    settle(writer)
    try {
      const load = loadavg()[0]
      const probe = await writesDuring(runPurge({ path, root: 1, timeoutMs: 10_000, report: true }), {
        lockProbe: true,
      })
      expect(probe.error).toBeUndefined()
      measured(
        `very large account: longest write ${probe.longest.toFixed(1)} ms, timer gap ${probe.gap.toFixed(1)} ms; ${probe.report}`,
      )
      const skip = overloaded(load)
      ctx.skip(skip !== undefined, skip && `${skip} (${probe.longest.toFixed(1)} ms measured)`)
      expect(probe.longest, probe.report).toBeLessThan(250)
      expect(probe.gap, probe.report).toBeLessThan(250)
    } finally {
      reader.close()
      writer.close()
    }
  }, 120_000)

  // The server asks again for a purge whose call it gave up on (its time limit), and the first
  // purge's worker goes on: the second request waits for it and has its outcome, and starts no worker
  // of its own, so two purges never take turns on the write lock.
  it('runs one purge for a root at a time, however often it is asked', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const before = purgeWorkersStarted()
    try {
      const first = purgeOnce({ path, root: 1, timeoutMs: 5000 })
      const second = purgeOnce({ path, root: 1, timeoutMs: 5000 })
      await Promise.all([first, second])
      expect(purgeWorkersStarted() - before).toBe(1)
      expect(writer.prepare('SELECT id FROM conversations').all()).toEqual([{ id: 2 }])
      // Once it has ended, a new request is a new purge.
      await purgeOnce({ path, timeoutMs: 5000 })
      expect(purgeWorkersStarted() - before).toBe(2)
    } finally {
      reader.close()
      writer.close()
    }
  })

  // Purges of two roots asked for at once run one after the other, never side by side: two workers
  // taking turns on the write lock would leave the harness's writes none of the pauses.
  it('runs purges of different roots one after the other', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const text = 'p'.repeat(400)
    const insert = writer.prepare('INSERT INTO entries (id, conversation_id, text) VALUES (?, 1, ?)')
    const record = writer.prepare('INSERT INTO record_ids (id) VALUES (?)')
    writer.exec('BEGIN')
    for (let id = 1000; id < 21_000; id++) {
      insert.run(id, text)
      record.run(id)
    }
    writer.exec('COMMIT')
    const before = purgeWorkersStarted()
    purgeWorkersAtOnce()
    try {
      await Promise.all([purgeOnce({ path, root: 1, timeoutMs: 5000 }), purgeOnce({ path, root: 2, timeoutMs: 5000 })])
      expect(purgeWorkersStarted() - before).toBe(2)
      expect(purgeWorkersAtOnce()).toBe(1)
      expect(writer.prepare('SELECT id FROM conversations').all()).toEqual([])
    } finally {
      reader.close()
      writer.close()
    }
  })

  // The checkpointer's thread, whose checkpoints all fail (the file isn't a database), is started
  // again after CHECKPOINT_FAILURES of them in a row; once stopped, none starts again, not even one
  // whose restart was waiting.
  it('starts its thread again after checkpoints keep failing, and not after it is stopped', async () => {
    const bad = join(dir, 'not-a-database')
    writeFileSync(bad, 'this is not a SQLite database, it only has the length of one '.repeat(100))
    let starts = 0
    const failures: number[] = []
    const stop = startCheckpointer(bad, 2, { started: () => starts++, failed: (n) => failures.push(n) })
    const until = Date.now() + 10_000
    while (starts < 3 && Date.now() < until) await new Promise((r) => setTimeout(r, 10))
    await stop()
    expect(starts).toBeGreaterThanOrEqual(3)
    expect(failures).toContain(10) // each thread counts its failures up to the bound before it restarts
    const stopped = starts
    await new Promise((r) => setTimeout(r, 100))
    expect(starts).toBe(stopped)

    // Stopped just as a restart was due: that restart doesn't happen.
    let again = 0
    let stopping: Promise<void> | undefined
    const stop2 = startCheckpointer(bad, 20, {
      started: () => again++,
      restarting: () => {
        stopping ??= stop2()
      },
    })
    const until2 = Date.now() + 10_000
    while (stopping === undefined && Date.now() < until2) await new Promise((r) => setTimeout(r, 10))
    await stopping
    await new Promise((r) => setTimeout(r, 200))
    expect(again).toBe(1)
  })

  // The checkpointer's thread doesn't keep a process alive: one that started it and has nothing
  // else to do ends on its own.
  it('lets the process end while the checkpointer runs', () => {
    const { writer, reader } = fixture()
    reader.close()
    writer.close()
    // A script of its own (not --eval: the worker would inherit --input-type and fail to start).
    const purge = new URL('./purge.ts', import.meta.url).href
    const script = join(dir, 'start.mjs')
    writeFileSync(
      script,
      `import { startCheckpointer } from ${JSON.stringify(purge)}
startCheckpointer(${JSON.stringify(path)}, 50, { started: () => console.log('started') })
`,
    )
    const child = spawnSync(process.execPath, [script], { timeout: 10_000, encoding: 'utf8' })
    expect(child.stdout).toContain('started')
    expect(child.signal, child.stderr).toBeNull()
    expect(child.status, child.stderr).toBe(0)
  })

  // A caller whose last work is to stop the checkpointer (a short-lived tool, a test) waits for it:
  // the thread, which doesn't keep the process up while it runs, does while it is being stopped, so
  // the process doesn't end with the stop unsettled.
  it('keeps the process up until the checkpointer has stopped', () => {
    const { writer, reader } = fixture()
    reader.close()
    writer.close()
    const purge = new URL('./purge.ts', import.meta.url).href
    const script = join(dir, 'stop.mjs')
    writeFileSync(
      script,
      `import { startCheckpointer } from ${JSON.stringify(purge)}
const stop = startCheckpointer(${JSON.stringify(path)}, 50)
await new Promise((resolve) => setTimeout(resolve, 200))
await stop()
console.log('stopped')
`,
    )
    const child = spawnSync(process.execPath, [script], { timeout: 10_000, encoding: 'utf8' })
    expect(child.signal, child.stderr).toBeNull()
    expect(child.status, child.stderr).toBe(0)
    expect(child.stdout).toContain('stopped')
  })

  it('rejects with the failure of a purge that is not about the log', async () => {
    await expect(runPurge({ path: join(dir, 'missing', 'agent.db'), root: 1, timeoutMs: 100 })).rejects.toThrow()
  })
})
