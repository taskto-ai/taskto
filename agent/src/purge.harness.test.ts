// The purge against a database Pi Durable wrote itself: what the unit tests' fixture stands in for
// (purge.test.ts) is checked here against the real thing, so the two can't drift apart.
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, statSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { DatabaseSync } from 'node:sqlite'
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context'
import { createModels } from '@earendil-works/pi-ai/models'
import { type ConversationId, createRegistry, defineDoc, Harness } from '@earendil-works/pi-durable'
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { Lifecycle } from './lifecycle.js'
import { installProviders, loadProviders } from './providers.js'
import { HARNESS_DB_OPTIONS, LIFECYCLE_KIND, purgeRecords, storedKind } from './purge.js'
import { compactOnce, openHarnessStorage } from './storage.js'

const context = BACKGROUND_CONTEXT

// A document whose earlier revisions Pi Durable deletes as it writes new ones (history: latest).
const Notes = defineDoc<{ text: string }>({
  kind: 'taskto.test.notes',
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ text: '' }),
  checkpointWhen: () => true,
})

// Markers in what the notes say, one per write: short ones, and long ones that SQLite keeps in
// overflow pages, which it frees when they are deleted.
const markers = Array.from({ length: 5 }, (_, i) => `withdrawn-marker-${i}-6f1c`)
const notes = markers.map((m, i) => (i % 2 === 0 ? m : `${m} ${'w'.repeat(6000)}`))

describe('the purge on a database Pi Durable wrote', () => {
  let dir: string
  let path: string
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'taskto-purge-harness-'))
    path = join(dir, 'agent.db')
  })
  afterEach(() => rmSync(dir, { recursive: true, force: true }))

  // The harness as the sidecar opens it (openHarnessStorage), or as a version before this one did
  // (Pi Durable's own opener, without secure_delete).
  const open = async (storage: 'sidecar' | 'older' = 'sidecar') => {
    const models = createModels()
    const catalog = installProviders(models, loadProviders(undefined))
    const harness = await Harness.open(
      storage === 'sidecar' ? await openHarnessStorage(path) : await openNodeSqliteStorage(path, HARNESS_DB_OPTIONS),
      { models, registry: createRegistry(), identity: { attribution: null } },
      context,
    )
    return { harness, model: catalog.default }
  }

  // As the sidecar does: a conversation with its lifecycle document, then retired. Before that, the
  // notes are written to a document one after another: Pi Durable keeps the latest revision and
  // deletes the one before at each write, on its own connection, as it does with a queue of input
  // whose items are withdrawn.
  const retired = async (notes: string[] = [], storage: 'sidecar' | 'older' = 'sidecar') => {
    const { harness, model } = await open(storage)
    const conv = await harness.createConversation(
      {
        ownership: { kind: 'ownerless' },
        agent: { model },
        init: async (tx, id) => {
          ;(await tx.doc(Lifecycle, id)).createdAt = Date.now()
        },
      },
      context,
    )
    for (const text of notes) {
      await conv.commit(async (tx) => {
        ;(await tx.doc(Notes, conv.id)).text = text
      }, context)
    }
    await conv.commit(async (tx) => {
      ;(await tx.doc(Lifecycle, conv.id)).retiredAt = Date.now()
    }, context)
    await conv.abort(context, { background: true })
    await harness.close(context)
    return conv.id
  }

  it('finds the lifecycle document by the kind Pi Durable stored', async () => {
    const id = await retired()
    const db = new DatabaseSync(path)
    try {
      const kinds = db
        .prepare("SELECT kind FROM documents WHERE scope_kind = 'conversation' AND owner_id = ?")
        .all(id) as { kind: string }[]
      expect(kinds.map((k) => k.kind)).toContain(storedKind(LIFECYCLE_KIND))
    } finally {
      db.close()
    }
  })

  // The reviewer's sequence: the purge stops after its first short transaction (a crash, a
  // deploy), and the sidecar starts again. The conversation is still retired, so it takes no input
  // and the next purge carries on; that one ends it.
  it('leaves a conversation retired when it stops after its first transaction', async () => {
    const id = await retired()
    const stop = new Error('stopped')
    expect(() =>
      purgeRecords(path, id, 1000, {
        afterSlice: () => {
          throw stop
        },
      }),
    ).toThrow(stop)

    let { harness } = await open()
    try {
      expect(await harness.conversation(id as ConversationId, context)).toBeDefined()
      expect((await harness.snapshot(Lifecycle, id as ConversationId, context))?.retiredAt).toBeGreaterThan(0)
    } finally {
      await harness.close(context)
    }

    purgeRecords(path, id, 1000)
    ;({ harness } = await open())
    try {
      expect(await harness.conversation(id as ConversationId, context)).toBeUndefined()
    } finally {
      await harness.close(context)
    }
  })

  // How many times each marker is in the database's file and its log, byte for byte. Read with
  // every connection closed (closing a descriptor of the file drops a process's locks on it).
  const remnants = () => {
    const bytes = [path, `${path}-wal`].filter(existsSync).map((p) => readFileSync(p).toString('latin1'))
    return markers.map((m) => bytes.reduce((n, b) => n + b.split(m).length - 1, 0))
  }

  // What Pi Durable deleted on its own connection before the purge (the earlier revisions) is gone
  // from the file too, not only what the purge deletes: the harness's connection sets secure_delete.
  it('leaves nothing of what was deleted before the purge in the file or the log', async () => {
    const id = await retired(notes)
    purgeRecords(path, id, 5000)
    expect(remnants()).toEqual(markers.map(() => 0))
  })

  // A database a version before this one wrote holds such remnants; compacting it once at start
  // leaves none, and isn't done again.
  it('compacts a database from before once, and leaves nothing of what was deleted then', async () => {
    const id = await retired(notes, 'older')
    expect(remnants().some((n) => n > 0)).toBe(true) // the older version left them
    expect(compactOnce(path, 1)).toBe('too-large') // bounded: a larger one is left for later
    expect(remnants().some((n) => n > 0)).toBe(true)
    expect(compactOnce(path)).toBe('compacted')
    expect(compactOnce(path)).toBe('done-before')
    purgeRecords(path, id, 5000)
    expect(remnants()).toEqual(markers.map(() => 0))
  })

  it('needs no compaction for a new database', () => {
    expect(compactOnce(path)).toBe('fresh')
  })

  const recorded = () => {
    const db = new DatabaseSync(path, { readOnly: true })
    try {
      return db.prepare("SELECT count(*) AS n FROM taskto_maintenance WHERE name = 'secure_delete_vacuum'").get()
    } finally {
      db.close()
    }
  }

  // A reader (a backup's snapshot) holds the log while the sidecar starts: the rewrite can't be
  // emptied out of the log, so the compaction isn't recorded done; it is at a start without one.
  it('defers the compaction while a reader holds the log', async () => {
    await retired(notes, 'older')
    const reader = new DatabaseSync(path)
    reader.exec('BEGIN')
    reader.prepare('SELECT count(*) FROM documents').get()
    try {
      expect(compactOnce(path)).toBe('deferred')
      expect(recorded()).toEqual({ n: 0 })
    } finally {
      reader.exec('COMMIT')
      reader.close()
    }
    expect(compactOnce(path)).toBe('compacted')
    // Only the latest note is left: the document still holds it, the earlier ones were deleted.
    expect(remnants()).toEqual(markers.map((_, i) => (i === markers.length - 1 ? 1 : 0)))
  })

  // After a hard stop the log can be large: finding the compaction done before doesn't checkpoint
  // it (a read-write connection's closing would, and the start would wait for it).
  it('looks for the compaction without checkpointing the log', async () => {
    await retired()
    expect(compactOnce(path)).toBe('compacted')
    // A log left as a hard stop leaves it: written, and its writer gone without checkpointing it
    // (another connection was open when it closed, and that one only reads).
    const keep = new DatabaseSync(path, { readOnly: true })
    keep.prepare('SELECT count(*) FROM documents').get()
    const writer = new DatabaseSync(path)
    writer.exec('PRAGMA wal_autocheckpoint = 0')
    writer.exec('CREATE TABLE IF NOT EXISTS filler (x TEXT)')
    const fill = writer.prepare('INSERT INTO filler (x) VALUES (?)')
    writer.exec('BEGIN')
    for (let i = 0; i < 2000; i++) fill.run('f'.repeat(1000))
    writer.exec('COMMIT')
    writer.close()
    keep.close()
    const before = statSync(`${path}-wal`).size
    expect(before).toBeGreaterThan(1_000_000)
    expect(compactOnce(path)).toBe('done-before')
    expect(statSync(`${path}-wal`).size).toBe(before)
  })

  // SQLite failing as it compacts (the disk full, the file read-only) defers it: the sidecar starts.
  it('defers the compaction when SQLite fails, rather than not starting', async () => {
    if (process.getuid?.() === 0) return // root writes a read-only file all the same
    await retired(notes, 'older')
    // SQLite makes the log and its index with the file's mode, so all three are given it back.
    const files = () => [path, `${path}-wal`, `${path}-shm`].filter(existsSync)
    chmodSync(path, 0o444)
    try {
      expect(compactOnce(path)).toBe('deferred')
    } finally {
      for (const f of files()) chmodSync(f, 0o644)
    }
    expect(compactOnce(path)).toBe('compacted')
  })
})
