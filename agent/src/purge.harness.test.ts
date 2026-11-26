// The purge against a database Pi Durable wrote itself: what the unit tests' fixture stands in for
// (purge.test.ts) is checked here against the real thing, so the two can't drift apart.
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { DatabaseSync } from 'node:sqlite'
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context'
import { createModels } from '@earendil-works/pi-ai/models'
import { type ConversationId, createRegistry, Harness } from '@earendil-works/pi-durable'
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { Lifecycle } from './lifecycle.js'
import { installProviders, loadProviders } from './providers.js'
import { HARNESS_DB_OPTIONS, LIFECYCLE_KIND, purgeRecords, storedKind } from './purge.js'

const context = BACKGROUND_CONTEXT

describe('the purge on a database Pi Durable wrote', () => {
  let dir: string
  let path: string
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'taskto-purge-harness-'))
    path = join(dir, 'agent.db')
  })
  afterEach(() => rmSync(dir, { recursive: true, force: true }))

  const open = async () => {
    const models = createModels()
    const catalog = installProviders(models, loadProviders(undefined))
    const harness = await Harness.open(
      await openNodeSqliteStorage(path, HARNESS_DB_OPTIONS),
      { models, registry: createRegistry(), identity: { attribution: null } },
      context,
    )
    return { harness, model: catalog.default }
  }

  // As the sidecar does: a conversation with its lifecycle document, then retired.
  const retired = async () => {
    const { harness, model } = await open()
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
})
