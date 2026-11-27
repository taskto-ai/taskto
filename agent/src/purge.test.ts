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
  isContention,
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

function measured(line: string): void {
  process.stderr.write(`${line}\n`)
}

function overloaded(before: number): string | undefined {
  const load = Math.max(before, loadavg()[0])
  const cores = availableParallelism()
  if (process.env.CI || load <= cores) return undefined
  return `skipped: the load average is ${load.toFixed(1)} on ${cores} cores, so this machine can't tell a stall of the purge's from its own; the bound is asserted when the load is at most the cores, and always in CI`
}

const LOG_FRAMES = 3 * SLICE_PAGES

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

  const manyTasksUnder3 = (writer: DatabaseSync) => {
    const task = writer.prepare('INSERT INTO tasks (id, conversation_id) VALUES (?, 3)')
    writer.exec('BEGIN')
    for (let id = 300_000; id < 320_000; id++) task.run(id)
    writer.exec('COMMIT')
  }
  const marksLeft = (writer: DatabaseSync) =>
    (writer.prepare('SELECT count(*) AS n FROM taskto_purge_marks WHERE root = 1 AND id <> 1').get() as { n: number }).n

  const settle = (writer: DatabaseSync) => {
    writer.exec('PRAGMA fullfsync = ON')
    writer.exec('PRAGMA checkpoint_fullfsync = ON')
    writer.exec('PRAGMA wal_checkpoint(TRUNCATE)')
  }

  const harnessLike = (db: DatabaseSync) => {
    db.exec(`PRAGMA wal_autocheckpoint = ${HARNESS_DB_OPTIONS.walAutoCheckpointPages}`)
    db.exec('PRAGMA synchronous = NORMAL')
  }

  const pending = () => {
    const db = new DatabaseSync(path, { timeout: 0 })
    try {
      const deadline = performance.now() + 5000
      for (;;) {
        const row = db.prepare('PRAGMA wal_checkpoint(NOOP)').get() as {
          busy: number
          log: number
          checkpointed: number
        }
        if (Number(row.busy) === 0) return Number(row.log) - Number(row.checkpointed)
        if (performance.now() > deadline) throw new Error('the log was never free to look at')
        Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 2)
      }
    } finally {
      db.close()
    }
  }

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

  const writesDuring = async (work: Promise<unknown>, options: { lockProbe?: boolean } = {}) => {
    const db = new DatabaseSync(path, { timeout: 5000 })
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
    let locked = 0
    let failure: unknown
    try {
      while (!done) {
        const t0 = performance.now()
        insert.run()
        longest = Math.max(longest, performance.now() - t0)
        writes++
        await new Promise((r) => setTimeout(r, 15))
      }
    } catch (error) {
      failure = error
    }
    clearInterval(timer)
    try {
      locked = probe ? await probe.stop() : 0
    } catch (error) {
      if (failure === undefined) failure = error
      else console.error('the lock probe failed too', error)
    } finally {
      db.close()
    }
    if (failure !== undefined) throw failure
    const took = performance.now() - began
    const error = await settled
    return { longest, gap, writes, took, error, locked, report: JSON.stringify({ lockHeld: locked, purge: report }) }
  }

  it('waits for a reader off the calling thread, which keeps running meanwhile', async () => {
    const { writer, reader } = fixture()
    const wait = 600
    try {
      const inline = await ticksDuring(() => purgeRecords(path, 2, wait))
      expect(inline.error).toBeInstanceOf(WalBusyError)
      expect(inline.ms).toBeGreaterThanOrEqual(wait * 0.8)
      expect(inline.ticks).toBe(0)

      const off = await ticksDuring(() => runPurge({ path, root: 1, timeoutMs: wait }))
      expect(off.error).toBeInstanceOf(WalBusyError)
      expect(off.ms).toBeGreaterThanOrEqual(wait * 0.8)
      expect(off.ticks).toBeGreaterThanOrEqual(Math.floor(wait / 10 / 3))
      expect(log()).toContain(WORD)

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

  it('leaves the write lock to other writes while it waits for a reader', async () => {
    const { writer, reader } = fixture()
    const wait = 800
    try {
      const probe = await writesDuring(runPurge({ path, root: 1, timeoutMs: wait }))
      expect(probe.error).toBeInstanceOf(WalBusyError)
      expect(probe.longest).toBeLessThan(wait / 4)
      expect(probe.writes).toBeGreaterThan(10)
    } finally {
      reader.close()
      writer.close()
    }
  })

  it('leaves the write lock to other writes while it deletes a large conversation', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const text = 'x'.repeat(800)
    writer.exec('BEGIN')
    const insert = writer.prepare('INSERT INTO entries (id, conversation_id, text) VALUES (?, 1, ?)')
    const record = writer.prepare('INSERT INTO record_ids (id) VALUES (?)')
    for (let id = 1000; id < 251_000; id++) {
      insert.run(id, text)
      record.run(id)
    }
    writer.exec('COMMIT')
    settle(writer)
    try {
      const probe = await writesDuring(runPurge({ path, root: 1, timeoutMs: 5000 }))
      expect(probe.error).toBeUndefined()
      expect(probe.longest).toBeLessThan(probe.took / 4)
      expect(probe.gap).toBeLessThan(probe.took / 4)
      expect(probe.writes).toBeGreaterThan(3)
      expect(writer.prepare('SELECT count(*) AS n FROM entries WHERE conversation_id = 1').get()).toEqual({ n: 0 })
      expect(writer.prepare('SELECT count(*) AS n FROM record_ids').get()).toEqual({ n: 3 })
    } finally {
      reader.close()
      writer.close()
    }
  }, 30_000)

  it('stops halfway and is finished by the next, which still finds every conversation', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
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

  it('takes what was written during its pauses', () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    manyTasksUnder3(writer)
    let wrote = false
    try {
      purgeRecords(path, 1, 1000, {
        afterSlice: () => {
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

  it('takes a revision written between the revisions and their document', () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
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
  }, 120_000)

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
      expect(probe.longest, probe.report).toBeLessThan(250)
      expect(probe.gap, probe.report).toBeLessThan(250)
    } finally {
      reader.close()
      writer.close()
    }
  }, 60_000)

  it('stops while it takes the tree apart and is finished by the next', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
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
      expect(rounds[0].ms).toBeGreaterThan(20)
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
    const { writer, reader } = fixture()
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
          if (rounds.length === 1) reader.exec('COMMIT')
        },
      })
      expect(rounds[0]).toBeGreaterThan(1000)
      expect(rounds.length).toBeGreaterThan(1)
      expect(rounds[rounds.length - 1]).toBeLessThanOrEqual(64)
      expect(log()).toBe('')
    } finally {
      db.close()
      reader.close()
      writer.close()
    }
  })

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
      expect(seen[0].backlog).toBeLessThanOrEqual(64)
      expect(seen[0].before).toBeLessThanOrEqual(64)
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
        for (let id = 2_000_000; id < 2_000_000 + tasks / 4; id++) {
          entry.run(id, body)
          record.run(id)
        }
      })
    const runs = { small: run(2_000, 4000), large: run(20_000, 4000), wide: run(400, 100_000) }
    for (const r of Object.values(runs)) {
      const { most, seen } = r
      expect(most('pages'), seen).toBeLessThanOrEqual(SLICE_PAGES)
      expect(Math.max(...r.frames), seen).toBeLessThanOrEqual(LOG_FRAMES)
      expect(r.finalFrames, seen).toBeLessThanOrEqual(LOG_FRAMES)
      expect(most('records'), seen).toBeLessThanOrEqual(CHUNK)
      expect(most('steps'), seen).toBeLessThanOrEqual(SLICE_STEPS)
      expect(most('changes'), seen).toBeLessThanOrEqual(3 * CHUNK)
    }
    expect(runs.large.final).toEqual(runs.small.final)
    expect(runs.large.final?.changes).toBeLessThanOrEqual(8)
    expect(runs.large.final?.pages).toBeLessThanOrEqual(4)
    expect(runs.large.slices.length).toBeGreaterThan(runs.small.slices.length * 5)
  }, 120_000)

  it('sizes each delete by the rows it takes when records change size', () => {
    const small = 'r'.repeat(200)
    const wide = 'w'.repeat(256 * 1024)
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

  it('takes a record larger than a transaction alone', () => {
    const huge = 'h'.repeat(4 * 1024 * 1024)
    const r = workOf('huge', (writer) => {
      const entry = writer.prepare('INSERT INTO entries (id, conversation_id, text) VALUES (?, 1, ?)')
      for (let i = 0; i < 10; i++) entry.run(2_000_000 + i, i === 5 ? huge : 'small')
    })
    const big = r.slices.filter((s) => s.pages > SLICE_PAGES)
    expect(big.length, r.seen).toBe(1)
    expect(big[0].records, r.seen).toBe(1)
  }, 120_000)

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
    let work: FinalWork | undefined
    try {
      purgeRecords(path, 1, 10_000, {
        final: (ms, w) => {
          last = ms
          work = w
        },
      })
      measured(`last transaction after 60,000 tasks: ${last.toFixed(1)} ms, ${JSON.stringify(work)}`)
      expect(work?.changes).toBeLessThanOrEqual(8)
      expect(work?.pages).toBeLessThanOrEqual(4)
      expect(writer.prepare('SELECT count(*) AS n FROM tasks').get()).toEqual({ n: 0 })
      expect(writer.prepare('SELECT count(*) AS n FROM taskto_purge_marks').get()).toEqual({ n: 0 })
    } finally {
      reader.close()
      writer.close()
    }
  }, 60_000)

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

  it('reads a busy checkpoint as not drained', () => {
    expect(framesLeft({ busy: 1, log: -1, checkpointed: -1 })).toBe(Number.POSITIVE_INFINITY)
    expect(framesLeft({ busy: 0, log: -1, checkpointed: -1 })).toBe(0)
    expect(framesLeft({ busy: 0, log: 10, checkpointed: 4 })).toBe(6)
    expect(framesLeft({ busy: 1, log: 10, checkpointed: 4 })).toBe(Number.POSITIVE_INFINITY)
  })

  it('reports a checkpoint that fails as a failure, not as a reader holding the log', () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const readOnly = new DatabaseSync(path, { readOnly: true })
    try {
      const began = performance.now()
      let error: unknown
      try {
        truncateWalWithin(readOnly, 2000)
      } catch (e) {
        error = e
      }
      expect(error).toBeInstanceOf(Error)
      expect(error).not.toBeInstanceOf(WalBusyError)
      expect(((error as { errcode?: number }).errcode ?? 0) & 0xff).toBe(10)
      expect(performance.now() - began).toBeLessThan(1000)
      expect(isContention({ errcode: 5 })).toBe(true)
      expect(isContention({ errcode: 517 })).toBe(true)
      expect(isContention({ errcode: 6 })).toBe(true)
      expect(isContention({ errcode: 778 })).toBe(false)
      expect(isContention(new Error('no code'))).toBe(false)
    } finally {
      readOnly.close()
      reader.close()
      writer.close()
    }
  })

  it('truncates only after a quick second look too', () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const more = writer.prepare('INSERT INTO entries (conversation_id, text) VALUES (2, ?)')
    const db = new DatabaseSync(path, { timeout: 1000 })
    let wrote = false
    let looked = 0
    const sinceLook: number[] = []
    try {
      truncateWalWithin(db, 20_000, {
        round: () => {
          if (!wrote) {
            wrote = true
            writer.exec('BEGIN')
            for (let i = 0; i < 100_000; i++) more.run('q'.repeat(400))
            writer.exec('COMMIT')
          }
          looked = performance.now()
        },
        truncating: () => sinceLook.push(performance.now() - looked),
      })
      expect(wrote).toBe(true)
      expect(sinceLook.length).toBeGreaterThan(0)
      for (const ms of sinceLook) expect(ms).toBeLessThanOrEqual(40)
      expect(log()).toBe('')
    } finally {
      db.close()
      reader.close()
      writer.close()
    }
  }, 60_000)

  it('does not truncate while another checkpoint is copying the log', async () => {
    const { writer, reader } = fixture()
    reader.exec('COMMIT')
    const more = writer.prepare('INSERT INTO entries (conversation_id, text) VALUES (2, ?)')
    writer.exec('BEGIN')
    for (let i = 0; i < 300_000; i++) more.run('q'.repeat(400))
    writer.exec('COMMIT')
    const times = new Float64Array(new SharedArrayBuffer(16))
    const flag = new Int32Array(new SharedArrayBuffer(4))
    const other = new Worker(
      `const { DatabaseSync } = require('node:sqlite')
       const { workerData } = require('node:worker_threads')
       const db = new DatabaseSync(workerData.path, { timeout: 0 })
       workerData.times[0] = performance.timeOrigin + performance.now()
       Atomics.store(workerData.flag, 0, 1)
       Atomics.notify(workerData.flag, 0)
       db.prepare('PRAGMA wal_checkpoint(PASSIVE)').get()
       workerData.times[1] = performance.timeOrigin + performance.now()
       db.close()`,
      { eval: true, workerData: { path, times, flag } },
    )
    const ended = new Promise((resolve) => other.once('exit', resolve))
    Atomics.wait(flag, 0, 0, 5000)
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 5)
    const db = new DatabaseSync(path, { timeout: 1000 })
    const rounds: { backlog: number; began: number; ended: number }[] = []
    const truncated: number[] = []
    try {
      truncateWalWithin(db, 20_000, {
        round: (backlog, ms) => {
          const now = performance.timeOrigin + performance.now()
          rounds.push({ backlog, began: now - ms, ended: now })
        },
        truncating: () => truncated.push(pending()),
      })
      const inside = rounds.filter((r) => r.began >= times[0] + 5 && r.ended <= times[1])
      for (const r of inside) expect(r.backlog).toBe(Number.POSITIVE_INFINITY)
      for (const p of truncated) expect(p).toBeLessThanOrEqual(64)
      expect(log()).toBe('')
    } finally {
      await ended
      db.close()
      reader.close()
      writer.close()
    }
  }, 60_000)

  it('keeps every transaction short on a very large account, and carries on after a stop', async (ctx) => {
    const load = loadavg()[0]
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
      started = performance.now()
      purgeRecords(path, 1, 10_000, {
        slice: (ms) => slices.push(ms),
        afterSlice: () => {
          if (firstSlice === Number.POSITIVE_INFINITY) firstSlice = performance.now() - started
        },
      })
      const took = performance.now() - started
      expect(writer.prepare('SELECT count(*) AS n FROM tasks').get()).toEqual({ n: 0 })
      expect(writer.prepare('SELECT count(*) AS n FROM taskto_purge_marks').get()).toEqual({ n: 0 })
      const longest = Math.max(...slices)
      measured(
        `very large account, retried: first transaction after ${firstSlice.toFixed(1)} ms, longest ${longest.toFixed(1)} ms, all in ${took.toFixed(0)} ms`,
      )
      const skip = overloaded(load)
      ctx.skip(skip !== undefined, skip)
      expect(firstSlice).toBeLessThan(200)
      expect(longest).toBeLessThan(100)
      expect(took).toBeLessThan(20_000)
    } finally {
      reader.close()
      writer.close()
    }
  }, 120_000)

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
      await purgeOnce({ path, timeoutMs: 5000 })
      expect(purgeWorkersStarted() - before).toBe(2)
    } finally {
      reader.close()
      writer.close()
    }
  })

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
    expect(failures).toContain(10)
    const stopped = starts
    await new Promise((r) => setTimeout(r, 100))
    expect(starts).toBe(stopped)

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

  it('lets the process end while the checkpointer runs', () => {
    const { writer, reader } = fixture()
    reader.close()
    writer.close()
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
