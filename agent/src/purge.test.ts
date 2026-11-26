import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { DatabaseSync } from 'node:sqlite'
import { pathToFileURL } from 'node:url'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { type PurgeJob, purgeOffThread, purgeRecords } from './purge.js'
import { WalBusyError } from './wal.js'

// The tables of the harness's database that the purge touches, as its migrations make them.
const SCHEMA = `
  CREATE TABLE record_ids (id INTEGER PRIMARY KEY, record_type TEXT NOT NULL) STRICT;
  CREATE TABLE conversations (id INTEGER PRIMARY KEY, owner_conversation_id INTEGER, owner_task_id INTEGER, record TEXT NOT NULL) STRICT;
  CREATE TABLE entries (id INTEGER PRIMARY KEY, conversation_id INTEGER NOT NULL, head INTEGER, commit_seq INTEGER NOT NULL, record TEXT NOT NULL) STRICT;
  CREATE TABLE tasks (id INTEGER PRIMARY KEY, conversation_id INTEGER NOT NULL, kind TEXT NOT NULL, record TEXT NOT NULL) STRICT;
  CREATE TABLE submissions (id INTEGER PRIMARY KEY, conversation_id INTEGER NOT NULL, record TEXT NOT NULL) STRICT;
  CREATE TABLE documents (id INTEGER PRIMARY KEY, scope_kind TEXT NOT NULL, owner_id INTEGER NOT NULL, record TEXT NOT NULL) STRICT;
  CREATE TABLE document_revisions (document_id INTEGER NOT NULL, seq INTEGER NOT NULL, record TEXT NOT NULL, PRIMARY KEY (document_id, seq)) STRICT;
`

describe('purgeRecords', () => {
  let dir: string
  let path: string
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'taskto-purge-rows-'))
    path = join(dir, 'agent.db')
    const db = new DatabaseSync(path)
    db.exec('PRAGMA journal_mode = WAL')
    db.exec(SCHEMA)
    // Conversation 1 with a sub-conversation (2) its task 10 started, and conversation 3, not theirs.
    db.exec(`INSERT INTO record_ids VALUES (1,'conversation'),(2,'conversation'),(3,'conversation'),(10,'task')`)
    db.exec(`INSERT INTO conversations VALUES (1,NULL,NULL,'{}'),(2,1,10,'{}'),(3,NULL,NULL,'{}')`)
    db.exec(`INSERT INTO tasks VALUES (10,1,'k','{}')`)
    const ids = db.prepare('INSERT INTO record_ids VALUES (?, ?)')
    const entry = db.prepare('INSERT INTO entries VALUES (?, ?, NULL, 1, ?)')
    for (let i = 0; i < 300; i++) {
      ids.run(100 + i, 'entry')
      entry.run(100 + i, i % 2 === 0 ? 1 : 2, '{"text":"secret"}')
    }
    ids.run(1000, 'entry')
    entry.run(1000, 3, '{"text":"kept"}')
    ids.run(2000, 'document')
    db.exec(`INSERT INTO documents VALUES (2000,'conversation',1,'{}')`)
    for (let i = 0; i < 450; i++) db.exec(`INSERT INTO document_revisions VALUES (2000,${i},'{"text":"secret"}')`)
    db.close()
  })
  afterEach(() => rmSync(dir, { recursive: true, force: true }))

  const count = (sql: string) => {
    const db = new DatabaseSync(path)
    try {
      return Number((db.prepare(sql).get() as { n: number | bigint }).n)
    } finally {
      db.close()
    }
  }

  it('deletes the conversation, its sub-conversations and every record, and no one else', () => {
    purgeRecords(path, 1, 1000)
    expect(count('SELECT count(*) AS n FROM conversations')).toBe(1)
    expect(count('SELECT count(*) AS n FROM tasks')).toBe(0)
    expect(count('SELECT count(*) AS n FROM entries')).toBe(1)
    expect(count('SELECT count(*) AS n FROM documents')).toBe(0)
    expect(count('SELECT count(*) AS n FROM document_revisions')).toBe(0)
    // The ids of what is kept stay, and no id of what went is left.
    expect(count('SELECT count(*) AS n FROM record_ids')).toBe(2)
  })

  it('keeps the conversation until the last step, so a purge that stopped is asked again and finishes', () => {
    const db = new DatabaseSync(path)
    // The deletion of one entry fails, partway through.
    db.exec(`CREATE TRIGGER stop BEFORE DELETE ON entries WHEN old.id = 350 BEGIN SELECT RAISE(ABORT, 'stopped'); END`)
    db.close()
    expect(() => purgeRecords(path, 1, 1000)).toThrow('stopped')
    // Not purged: the conversations and their tasks are still there, and the batches before the
    // failure went in transactions of their own, with their ids.
    expect(count('SELECT count(*) AS n FROM conversations WHERE id IN (1, 2)')).toBe(2)
    expect(count('SELECT count(*) AS n FROM tasks')).toBe(1)
    expect(count('SELECT count(*) AS n FROM document_revisions')).toBe(0)
    expect(count('SELECT count(*) AS n FROM documents')).toBe(0)
    expect(count('SELECT count(*) AS n FROM record_ids WHERE id = 2000')).toBe(0)
    expect(
      count(
        "SELECT count(*) AS n FROM record_ids WHERE record_type = 'entry' AND id < 1000 AND id NOT IN (SELECT id FROM entries)",
      ),
    ).toBe(0)
    const again = new DatabaseSync(path)
    again.exec('DROP TRIGGER stop')
    again.close()
    purgeRecords(path, 1, 1000)
    expect(count('SELECT count(*) AS n FROM conversations')).toBe(1)
    expect(count('SELECT count(*) AS n FROM entries')).toBe(1)
    expect(count('SELECT count(*) AS n FROM record_ids')).toBe(2)
  })
})

const job: PurgeJob = { op: 'truncate', dbPath: '/nonexistent/x.db', walBusyMs: 10 }

// How the thread's answer becomes the promise's: the sidecar's route turns a WalBusyError into the
// 503 that says the purge isn't done, and anything else into a 500.
describe('purgeOffThread', () => {
  let dir: string
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'taskto-purge-'))
  })
  afterEach(() => rmSync(dir, { recursive: true, force: true }))

  const worker = (name: string, source: string) => {
    const path = join(dir, name)
    writeFileSync(path, source)
    return pathToFileURL(path)
  }

  it('settles when the thread says it is done, and runs off the calling thread', async () => {
    const url = worker(
      'ok.mjs',
      `import { parentPort, threadId } from 'node:worker_threads'
parentPort.postMessage({ ok: true, threadId })`,
    )
    await expect(purgeOffThread(job, url)).resolves.toBeUndefined()
  })

  it('does not block the calling thread while the work runs', async () => {
    const url = worker(
      'slow.mjs',
      `import { parentPort } from 'node:worker_threads'
Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 300)
parentPort.postMessage({ ok: true })`,
    )
    let ticks = 0
    const timer = setInterval(() => ticks++, 10)
    try {
      await purgeOffThread(job, url)
    } finally {
      clearInterval(timer)
    }
    // A thread that slept 300 ms here would have let no timer fire.
    expect(ticks).toBeGreaterThan(5)
  })

  it('rejects with WalBusyError when a reader holds the log', async () => {
    const url = worker(
      'busy.mjs',
      `import { parentPort } from 'node:worker_threads'
parentPort.postMessage({ ok: false, busy: true, message: 'a reader holds the log' })`,
    )
    await expect(purgeOffThread(job, url)).rejects.toBeInstanceOf(WalBusyError)
  })

  it('rejects with the failure, a crash, or a thread that ends without answering', async () => {
    const failed = worker(
      'failed.mjs',
      `import { parentPort } from 'node:worker_threads'
parentPort.postMessage({ ok: false, busy: false, message: 'disk full' })`,
    )
    await expect(purgeOffThread(job, failed)).rejects.toThrow('disk full')
    const crashed = worker('crashed.mjs', `throw new Error('boom')`)
    await expect(purgeOffThread(job, crashed)).rejects.toThrow('boom')
    const silent = worker('silent.mjs', `// ends at once`)
    await expect(purgeOffThread(job, silent)).rejects.toThrow('before it answered')
  })
})
