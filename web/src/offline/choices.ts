// What a person does with a change that needs them (docs/offline.md, "Conflicts and what doesn't
// fit"): keep the server's version, apply their change on top of it, edit it and try again, restore
// a record that was deleted and apply the change to it, or repair what no longer fits the app.
//
// Nothing here sends a change: each of these puts the item back in the queue with a new request
// (a new operation key, the revision it is now based on), and the engine sends it as it sends the
// rest, as the account that made it. Each is applied in the item's own transaction, together with
// the downloaded copy it changes, and only to an item that still needs attention and is still the
// request the person was shown: a tab that moved it on meanwhile wins, and what the person chose
// is then not applied (`false`). Nothing is overwritten without being shown: a conflict's choices
// are made while the server's record is on screen.

import { ApiError, api } from '../api'
import { assertSignedIn } from './account'
import { downloadApp } from './download'
import { applyUpdate, fieldsOf, parseField, parseInsert, parseUpdate, requireFields, withRecord } from './local'
import type { Runtime } from './runtime'
import {
  CopyChanged,
  type Download,
  ItemChanged,
  kindOf,
  NotWriter,
  type OutboxItem,
  type Problem,
  type Seen,
  type SeenAs,
  seenOf,
  versionOf,
} from './store'
import { NOT_WRITER, newOperation } from './transport'

type Definition = Download['definition']

// An item put back in the queue as a new request: everything that said why it was waiting for the
// person is cleared.
function requeued(i: OutboxItem): OutboxItem {
  return {
    ...i,
    operation: newOperation(),
    state: 'queued',
    attempts: 0,
    nextAttemptAt: undefined,
    attention: undefined,
    error: undefined,
    conflict: undefined,
    problems: undefined,
    restore: undefined,
    unconfirmed: undefined,
    unconfirmedBefore: undefined,
    lease: undefined,
  }
}

const revisionOf = (current: Record<string, unknown> | null | undefined): number | undefined =>
  typeof current?._revision === 'number' ? current._revision : undefined

// What the person chose was made on an item that is not what is stored now (another tab changed
// it): nothing is applied, and they review it again.
const changedElsewhere = () => new ApiError(409, 'this changed in another tab', { code: 'offline_item_changed' })

// The record moved on since the person was shown it: nothing is applied, and the screen shows it
// as it is now.
export const REVIEW_AGAIN = 'offline_review_again'
const reviewAgain = () => new ApiError(409, 'the record changed again', { code: REVIEW_AGAIN })

// Only the tab that holds the offline writer chooses (writer.ts): the others show the queue.
const notWriter = () => new ApiError(409, 'changes are being made in another tab', { code: NOT_WRITER })

// What was read from the server was read for a copy of the app that is not the one held now.
const copyChanged = () => new ApiError(409, 'the downloaded copy changed', { code: 'offline_copy_changed' })

// Runs a choice, which the store refuses when the item is not the one the person was shown, when
// this tab is not the writer, or when the copy it read for was replaced.
async function chosen<R>(go: () => Promise<R>): Promise<R> {
  try {
    return await go()
  } catch (e) {
    if (e instanceof ItemChanged) throw changedElsewhere()
    if (e instanceof NotWriter) throw notWriter()
    if (e instanceof CopyChanged) throw copyChanged()
    throw e
  }
}

// Refuses at once, before the server is asked anything, a choice made on an item that has moved
// on, or in a tab that is not the writer; the store checks it again in the transaction that
// applies the choice. Answers the generation of the downloaded copy as it is now, which the choice
// is applied against (`resolve`'s `copy`): taken before the server is read, so that a copy replaced
// while the answer was on its way (a revalidation that narrowed what the person reads) is never
// written over with what was read for the old one.
async function stillShown(runtime: Runtime, item: OutboxItem, seen: Seen): Promise<number | null> {
  if (!runtime.writer.holds()) throw notWriter()
  const stored = await runtime.store.getItem(item.id)
  if (!stored || versionOf(stored) !== seen.version || stored.operation !== seen.operation) throw changedElsewhere()
  return runtime.store.downloadGeneration(item)
}

// The person's discard of an item that was never acknowledged, as it was shown when they were
// asked (its version, its operation and its state): nothing else is removed.
export async function discard(runtime: Runtime, item: OutboxItem, seen: SeenAs): Promise<void> {
  if (!runtime.writer.holds()) throw notWriter()
  try {
    await chosen(() => runtime.store.discardItem(item.id, seen))
  } catch (e) {
    // The store refused and every list of this tab reads again (`OfflineStore.keep`). When the
    // item is on its way to the server (or the server has it), the person is told that, which is
    // the truth, and not only that it changed.
    if (e instanceof ApiError && e.code === 'offline_item_changed') {
      const now = await runtime.store.getItem(item.id).catch(() => undefined)
      if (now && (now.state === 'syncing' || now.state === 'synced' || now.unconfirmed))
        throw new ApiError(409, 'the record is being sent', { code: 'offline_item_sending' })
    }
    throw e
  }
  // A later change to the same record that waited for this one is sendable now.
  await runtime.engine.run()
}

// A record as the server has it now, for the person who is signed in: read at the moment it is
// asked for, under the access they have now, never remembered from an earlier answer (what a
// conflict was answered with may be a record they can no longer read, or have been shown before
// the app changed). `current` is an item (its id, `_revision` and fields), or null: the person may
// not read it (it is not theirs any more), or it is deleted (`deleted`).
export type ServerRecord = { current: Record<string, unknown> | null; deleted: boolean }

export async function readCurrent(runtime: Runtime, item: OutboxItem): Promise<ServerRecord> {
  try {
    const answer = await api<{
      record: { id: number; revision: number; data: Record<string, unknown>; deleted_at?: string | null }
    }>(`${item.base}/records/${item.record.id}/history`, { headers: runtime.engine.ownerHeaders() })
    // What the server answered is only acted on while this account is the one signed in here.
    assertSignedIn(runtime.store.user)
    const r = answer.record
    if (r.deleted_at) return { current: null, deleted: true }
    return { current: { id: r.id, _revision: r.revision, ...r.data }, deleted: false }
  } catch (e) {
    assertSignedIn(runtime.store.user)
    // The server doesn't show it to this person (403, 404): not readable, which is an answer. Any
    // other failure (no connection, the wrong account) is not, and nothing is decided from it.
    if (e instanceof ApiError && (e.status === 403 || e.status === 404)) return { current: null, deleted: false }
    throw e
  }
}

// Keeps the server's version: the change is discarded, and the copy on this device holds the record
// as the server has it right now (or no longer holds it: deleted, or not the person's to read: what
// was cached of it is dropped, never written back).
export async function keepServer(runtime: Runtime, item: OutboxItem, seen: Seen = seenOf(item)): Promise<boolean> {
  const generation = await stillShown(runtime, item, seen)
  const server = item.attention === 'conflict' ? await readCurrent(runtime, item) : null
  return chosen(() =>
    runtime.store.resolve(
      item.id,
      seen,
      (i, copy) => {
        if (i.attention !== 'conflict' && i.attention !== 'deleted') return undefined
        return { item: null, copy: copy && withRecord(copy, i.record.id, server?.current ?? null) }
      },
      { copy: generation },
    ),
  )
}

// Applies the person's change on top of the record as the server has it now: the same change,
// based on the revision they have just been shown. (A delete is made anyway.) The record is read
// at this moment: if it is not at the revision `shown` says, the person never saw this one, and
// nothing is applied (REVIEW_AGAIN); a change the person may not read the record of is theirs to
// discard.
export async function reapply(
  runtime: Runtime,
  item: OutboxItem,
  shown: Record<string, unknown> | null = item.conflict?.current ?? null,
  seen: Seen = seenOf(item),
): Promise<boolean> {
  const generation = await stillShown(runtime, item, seen)
  const server = await readCurrent(runtime, item)
  const current = await readable(runtime, item, seen, server, generation)
  const revision = revisionOf(current)
  if (revision === undefined || revision !== revisionOf(shown)) throw reviewAgain()
  const done = await chosen(() =>
    runtime.store.resolve(
      item.id,
      seen,
      (i, copy) => {
        if (i.attention !== 'conflict') return undefined
        return {
          item: { ...requeued(i), record: { ...i.record, revision }, original: fieldsOf(current) },
          copy: copy && withRecord(copy, i.record.id, current),
        }
      },
      { copy: generation },
    ),
  )
  if (done) await runtime.engine.run()
  return done
}

// The record as the server has it, when the person may read it and it is live. When it is not:
// deleted (the change is a change to a deleted record now), or not the person's to read, what was
// cached of it is gone from the copy, and nothing is applied.
async function readable(
  runtime: Runtime,
  item: OutboxItem,
  seen: Seen,
  server: ServerRecord,
  generation: number | null,
): Promise<Record<string, unknown>> {
  if (server.current) return server.current
  await chosen(() =>
    runtime.store.resolve(
      item.id,
      seen,
      (i, copy) => {
        if (i.attention !== 'conflict') return undefined
        const next: OutboxItem = server.deleted
          ? { ...i, attention: 'deleted', conflict: undefined, restore: undefined }
          : { ...i, conflict: { current: null } }
        return { item: next, copy: copy && withRecord(copy, i.record.id, null) }
      },
      { copy: generation },
    ),
  )
  throw server.deleted ? reviewAgain() : new ApiError(409, 'not readable', { code: 'offline_record_unreadable' })
}

// What is not right with an item's input against a definition: each value that doesn't fit its
// field, a field the app no longer has, and a field that is required now. `base` is the record's
// fields an update changes (an insert starts from none).
export function problemsOf(
  def: Definition,
  kind: 'insert' | 'update' | 'delete',
  context: Record<string, unknown>,
  base: Record<string, unknown> = {},
): Problem[] {
  if (kind === 'delete') return []
  const byName = new Map(def.collection.fields.map((f) => [f.name, f]))
  const out: Problem[] = []
  const values: Record<string, unknown> = kind === 'update' ? { ...base } : {}
  for (const [name, raw] of Object.entries(context)) {
    if (name === 'id' && kind === 'update') continue
    const f = byName.get(name)
    if (!f) {
      out.push({ field: name, code: 'offline_field_removed' })
      continue
    }
    try {
      const v = parseField(f, raw)
      if (v === undefined) delete values[name]
      else values[name] = v
    } catch (e) {
      if (e instanceof ApiError) out.push({ field: name, code: e.code ?? 'bad_request', params: e.params })
    }
  }
  try {
    requireFields(def, values)
  } catch (e) {
    if (e instanceof ApiError && !out.some((p) => p.field === (e.params as { field?: string } | undefined)?.field)) {
      out.push({
        field: String((e.params as { field?: string } | undefined)?.field ?? ''),
        code: e.code ?? 'bad_request',
        params: e.params,
      })
    }
  }
  return out
}

// The app as it is live now, for repairing an item against it: downloaded again (guarded as every
// download is: for this account's database, and written only while the browser is still signed in
// as them), which refreshes the copy too. What is queued stays as it is.
export async function refreshDefinition(runtime: Runtime, item: OutboxItem): Promise<Download> {
  return downloadApp(runtime.store, item)
}

// The change the person's edited values make, against the record as it is (`base`): an update
// sends only the fields the person changed (`changed`: theirs from the start, and those they edited)
// that differ from the record as it is now, so what they didn't touch is never written (it is the
// latest server record's, whatever the form showed when it was opened: base, mine, current); an
// insert sends all that were entered. Refused, with the server's words, if it doesn't fit.
export function contextFor(
  kind: 'insert' | 'update' | 'delete',
  def: Definition,
  form: Record<string, unknown>,
  base: Record<string, unknown>,
  kept: Record<string, unknown>,
  id: number,
  changed?: readonly string[],
): Record<string, unknown> {
  const context: Record<string, unknown> = { ...kept }
  for (const f of def.collection.fields) {
    if (f.type === 'attachment') continue
    const value = form[f.name]
    if (kind === 'insert') {
      if (value !== undefined && value !== '' && value !== null) context[f.name] = value
    } else if (changed !== undefined && !changed.includes(f.name)) {
    } else if (!same(value, base[f.name])) {
      context[f.name] = value === undefined ? '' : value
    }
  }
  if (kind === 'update') {
    context.id = id
    const changes = parseUpdate(def, context)
    if (Object.keys(changes).length === 0)
      throw new ApiError(422, 'nothing to change', { code: 'offline_nothing_to_change' })
    applyUpdate(def, base, changes)
  } else {
    parseInsert(def, context)
  }
  return context
}

const same = (a: unknown, b: unknown) => (a ?? '') === (b ?? '') || String(a ?? '') === String(b ?? '')

// Saves the person's edited values as the item's new request, against `def` (the app as it is
// live now): its release and definition version are the ones it is now made for, and an update is
// based on the record as the server has it if that is known. The record of a conflict is read at
// this moment, and the form was made from `shown`: if it is not at that revision the person never
// saw this one, and nothing is saved (REVIEW_AGAIN). Rejects with the server's words if the values
// don't fit. For an update, `changed` names the fields the person changed: only those are applied,
// onto the record as the server has it now.
export async function saveEdit(
  runtime: Runtime,
  item: OutboxItem,
  form: Record<string, unknown>,
  live: Download,
  shown: Record<string, unknown> | null = item.conflict?.current ?? null,
  seen: Seen = seenOf(item),
  changed?: readonly string[],
): Promise<boolean> {
  const kind = kindOf(item)
  const generation = await stillShown(runtime, item, seen)
  let current: Record<string, unknown> | undefined
  if (kind === 'update' && item.attention === 'conflict') {
    current = await readable(runtime, item, seen, await readCurrent(runtime, item), generation)
    if (revisionOf(current) !== revisionOf(shown)) throw reviewAgain()
  }
  const base = kind === 'update' ? (current ? fieldsOf(current) : (item.original ?? {})) : {}
  const kept = Object.fromEntries(
    Object.entries(item.action.context).filter(([name]) => item.attachments.some((a) => a.field === name)),
  )
  const context = contextFor(kind, live.definition, form, base, kept, item.record.id, changed)
  const revision = revisionOf(current)
  const done = await chosen(() =>
    runtime.store.resolve(
      item.id,
      seen,
      (i, copy) => {
        if (
          i.attention === 'access' ||
          i.attention === 'archived' ||
          i.attention === 'photo' ||
          i.attention === 'gave_up'
        ) {
          return undefined
        }
        const next = requeued({
          ...i,
          release: live.release,
          schema: live.definition.definition_version,
          action: { ...i.action, context },
        })
        if (kind === 'update') {
          next.record = { ...i.record, revision: revision ?? i.record.revision }
          if (current) next.original = fieldsOf(current)
        }
        return { item: next, copy: copy && current ? withRecord(copy, i.record.id, current) : copy }
      },
      { copy: generation },
    ),
  )
  if (done) await runtime.engine.run()
  return done
}

// A deleted record as the server lists it: the revision of its deletion, who and when.
export type Deleted = { revision: number; at: string | null; by: string | null }

// Asks the server whether the item's record is among the deleted ones the person may read, and its
// deletion's revision, which restoring needs. Null when it isn't (it is gone for good, or the
// person may not read it).
export async function findDeleted(runtime: Runtime, item: OutboxItem): Promise<Deleted | null> {
  const list = await api<{
    records: { id: number; revision: number; deleted_at: string | null; deleted_by: { name?: string } | null }[]
  }>(`${item.base}/records/deleted`, { headers: runtime.engine.ownerHeaders() })
  // What the server answered is only acted on while this account is the one signed in here.
  assertSignedIn(runtime.store.user)
  const found = list.records.find((r) => r.id === item.record.id)
  return found ? { revision: found.revision, at: found.deleted_at, by: found.deleted_by?.name ?? null } : null
}

// Chooses to restore a record that was deleted on the server and apply the person's change to
// it. The deletion is read again at this moment: if it is not the one the person was shown (it
// was restored, or deleted again at a newer revision), nothing is applied (REVIEW_AGAIN). The
// restore is the engine's first step when it sends the item, and the change goes on top of the
// revision the restore gives; if the restore is refused (the person may not), the item says so
// and the person discards it. Never done by itself: only this choice brings a record back.
export async function restoreAndApply(
  runtime: Runtime,
  item: OutboxItem,
  deleted: Deleted,
  seen: Seen = seenOf(item),
): Promise<boolean> {
  await stillShown(runtime, item, seen)
  const now = await findDeleted(runtime, item)
  if (!now || now.revision !== deleted.revision) throw reviewAgain()
  const done = await chosen(() =>
    runtime.store.resolve(item.id, seen, (i) => {
      if (i.attention !== 'deleted' || kindOf(i) !== 'update') return undefined
      return {
        item: {
          ...requeued(i),
          restore: { revision: now.revision },
          // A restore makes the next revision, which the change is based on.
          record: { ...i.record, revision: now.revision + 1 },
        },
      }
    }),
  )
  if (done) await runtime.engine.run()
  return done
}
