import { copyFileSync, mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { DatabaseSync } from 'node:sqlite'
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context'
import { createModels } from '@earendil-works/pi-ai/models'
import { type ConversationId, createRegistry, Harness } from '@earendil-works/pi-durable'
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { conversationState, Lifecycle } from './lifecycle.js'
import { installProviders, loadProviders } from './providers.js'
import { purgeRecords } from './purge.js'
import { truncateWalAt, WalBusyError } from './wal.js'

// A conversation made and retired the way the sidecar's routes do it, in a real Pi Durable database,
// then purged with the purge stopped after each transaction in turn (the process exits between
// batches). What the purge's route reads to recognize a retired conversation must survive every stop,
// and the retry must always finish.

const context = BACKGROUND_CONTEXT
const WORD = 'doomed-marmalade-7731'

async function openHarness(path: string) {
  const models = createModels()
  const catalog = installProviders(models, loadProviders(undefined))
  const harness = await Harness.open(
    await openNodeSqliteStorage(path),
    { models, registry: createRegistry(), identity: { attribution: null } },
    context,
  )
  return { harness, catalog }
}

// POST /conversations, some turns, then POST /conversations/:id/retire.
async function retiredConversation(path: string, turns: number): Promise<ConversationId> {
  const { harness, catalog } = await openHarness(path)
  try {
    const created = await harness.createConversation(
      {
        ownership: { kind: 'ownerless' },
        agent: { model: catalog.default },
        init: async (tx, id) => {
          ;(await tx.doc(Lifecycle, id)).createdAt = Date.now()
        },
      },
      context,
    )
    for (let i = 0; i < turns; i++) {
      const submission = await created.submit({ type: 'input', content: `${WORD} ${i}` }, context)
      await submission.wait(context)
    }
    await created.commit(async (tx) => {
      ;(await tx.doc(Lifecycle, created.id)).retiredAt = Date.now()
    }, context)
    await created.abort(context)
    return created.id
  } finally {
    await harness.close(context)
  }
}

const TABLES = ['conversations', 'tasks', 'entries', 'submissions', 'documents', 'document_revisions']

describe('purging a retired conversation that a Pi Durable database holds', () => {
  let dir: string
  let fixture: string
  let id: ConversationId
  let created: number[] // every record id the conversation made
  beforeEach(async () => {
    dir = mkdtempSync(join(tmpdir(), 'taskto-purge-retired-'))
    fixture = join(dir, 'fixture.db')
    id = await retiredConversation(fixture, 8)
    const db = new DatabaseSync(fixture)
    db.exec('PRAGMA wal_checkpoint(TRUNCATE)')
    created = (db.prepare('SELECT id FROM record_ids').all() as { id: number }[]).map((r) => Number(r.id))
    db.close()
  }, 60_000)
  afterEach(() => rmSync(dir, { recursive: true, force: true }))

  const rowsOf = (path: string) => {
    const db = new DatabaseSync(path)
    try {
      const n = (sql: string) => Number((db.prepare(sql).get() as { n: number | bigint }).n)
      return {
        conversations: n(`SELECT count(*) AS n FROM conversations WHERE id = ${id}`),
        tasks: n(`SELECT count(*) AS n FROM tasks WHERE conversation_id = ${id}`),
        entries: n(`SELECT count(*) AS n FROM entries WHERE conversation_id = ${id}`),
        submissions: n(`SELECT count(*) AS n FROM submissions WHERE conversation_id = ${id}`),
        documents: n(`SELECT count(*) AS n FROM documents WHERE scope_kind = 'conversation' AND owner_id = ${id}`),
        revisions: n(
          `SELECT count(*) AS n FROM document_revisions WHERE document_id IN (SELECT id FROM documents WHERE scope_kind = 'conversation' AND owner_id = ${id})`,
        ),
        recordIds: n(`SELECT count(*) AS n FROM record_ids WHERE id IN (${created.join(',')}) AND id <> 0`),
      }
    } finally {
      db.close()
    }
  }

  // How many transactions a purge of the fixture takes.
  const transactions = () => {
    const path = join(dir, 'count.db')
    copyFileSync(fixture, path)
    let total = 0
    purgeRecords(path, id, 1000, (n) => {
      total = n
    })
    return total
  }

  it('has rows in every table the purge deletes from, in more than a few batches', () => {
    const rows = rowsOf(fixture)
    for (const table of ['conversations', 'tasks', 'entries', 'submissions', 'documents', 'revisions'] as const) {
      expect(rows[table], table).toBeGreaterThan(0)
    }
    expect(TABLES.length).toBe(6)
    expect(transactions()).toBeGreaterThan(6)
  })

  it('is still recognized as retired after every transaction, and a retry always finishes and leaves nothing', async () => {
    const total = transactions()
    for (let stop = 1; stop <= total; stop++) {
      const path = join(dir, `stop-${stop}.db`)
      copyFileSync(fixture, path)
      expect(
        () =>
          purgeRecords(path, id, 1000, (n) => {
            if (n === stop) throw new Error('stopped')
          }),
        `stopped after transaction ${stop}`,
      ).toThrow('stopped')

      // The server asks again: the sidecar (a new process) looks the conversation up as the route does.
      const { harness } = await openHarness(path)
      const state = await conversationState(harness, id, context)
      await harness.close(context)
      const left = rowsOf(path)
      // Until the last transaction commits, the conversation is there and recognized as retired;
      // after it, it is gone, which the route answers by emptying the log.
      if (stop < total) {
        expect(state, `after transaction ${stop} of ${total}`).toBe('retired')
        expect(left.conversations, `after transaction ${stop}`).toBe(1)
      } else {
        expect(state).toBe('unknown')
        expect(left, `after the last transaction`).toEqual({
          conversations: 0,
          tasks: 0,
          entries: 0,
          submissions: 0,
          documents: 0,
          revisions: 0,
          recordIds: 0,
        })
      }

      // The retry finishes (the route: purge while it is retired, truncate once it is gone).
      if (state === 'retired') purgeRecords(path, id, 1000)
      else truncateWalAt(path, 1000)
      expect(rowsOf(path), `after the retry of ${stop}`).toEqual({
        conversations: 0,
        tasks: 0,
        entries: 0,
        submissions: 0,
        documents: 0,
        revisions: 0,
        recordIds: 0,
      })
      const { harness: again } = await openHarness(path)
      expect(await conversationState(again, id, context)).toBe('unknown')
      await again.close(context)
    }
  }, 120_000)

  it('is not done until the log is truncated, whichever step the purge stopped at', () => {
    const total = transactions()
    for (const stop of [1, Math.floor(total / 2), total]) {
      const path = join(dir, `wal-${stop}.db`)
      copyFileSync(fixture, path)
      // A reader pinned before the purge started, as a watch or a harness read is, holds the log.
      const reader = new DatabaseSync(path)
      reader.exec('BEGIN')
      reader.prepare('SELECT count(*) FROM record_ids').get()
      expect(() =>
        purgeRecords(path, id, 1000, (n) => {
          if (n === stop) throw new Error('stopped')
        }),
      ).toThrow('stopped')

      // Not success, whatever is left: the retry can't empty the log.
      expect(() => purgeRecords(path, id, 150)).toThrow(WalBusyError)
      // The rows are gone by now, so the route's retry is the truncation alone, and it isn't done either.
      expect(rowsOf(path).conversations).toBe(0)
      expect(() => truncateWalAt(path, 150)).toThrow(WalBusyError)

      reader.exec('COMMIT')
      reader.close()
      truncateWalAt(path, 1000)
      let wal = ''
      try {
        wal = readFileSync(`${path}-wal`).toString('latin1')
      } catch {
        // no log file at all: nothing to hold the words
      }
      expect(wal).not.toContain(WORD)
    }
  }, 60_000)
})
