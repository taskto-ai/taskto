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
import { type Download, kindOf, type OutboxItem, type Problem } from './store'
import { newOperation } from './transport'

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
    lease: undefined,
  }
}

const currentRevision = (current: Record<string, unknown> | null | undefined): number | undefined =>
  typeof current?._revision === 'number' ? current._revision : undefined

// Keeps the server's version: the change is discarded, and the copy on this device holds the record
// as the server has it (or no longer holds it: deleted, or not the person's to read).
export async function keepServer(runtime: Runtime, item: OutboxItem): Promise<boolean> {
  return runtime.store.resolve(item.id, item.operation, (i, copy) => {
    if (i.attention !== 'conflict' && i.attention !== 'deleted') return undefined
    const current = i.attention === 'conflict' ? i.conflict?.current : null
    return { item: null, copy: copy && withRecord(copy, i.record.id, current ?? null) }
  })
}

// Applies the person's change on top of the record as the server has it now: the same change,
// based on the revision they have just been shown. (A delete is made anyway.) Needs the record as
// the server answered it: a change the person may not read the record of is theirs to discard.
export async function reapply(runtime: Runtime, item: OutboxItem): Promise<boolean> {
  const done = await runtime.store.resolve(item.id, item.operation, (i, copy) => {
    const current = i.conflict?.current
    const revision = currentRevision(current)
    if (i.attention !== 'conflict' || !current || revision === undefined) return undefined
    return {
      item: { ...requeued(i), record: { ...i.record, revision }, original: fieldsOf(current) },
      copy: copy && withRecord(copy, i.record.id, current),
    }
  })
  if (done) await runtime.engine.run()
  return done
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
// sends only the fields that differ from it, so what they didn't touch is not overwritten; an
// insert sends all that were entered. Refused, with the server's words, if it doesn't fit.
export function contextFor(
  kind: 'insert' | 'update' | 'delete',
  def: Definition,
  form: Record<string, unknown>,
  base: Record<string, unknown>,
  kept: Record<string, unknown>,
  id: number,
): Record<string, unknown> {
  const context: Record<string, unknown> = { ...kept }
  for (const f of def.collection.fields) {
    if (f.type === 'attachment') continue
    const value = form[f.name]
    if (kind === 'insert') {
      if (value !== undefined && value !== '' && value !== null) context[f.name] = value
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
// based on the record as the server has it if that is known. Rejects with the server's words if
// the values don't fit.
export async function saveEdit(
  runtime: Runtime,
  item: OutboxItem,
  form: Record<string, unknown>,
  live: Download,
): Promise<boolean> {
  const kind = kindOf(item)
  const current = item.conflict?.current ?? undefined
  const base = kind === 'update' ? (current ? fieldsOf(current) : (item.original ?? {})) : {}
  const kept = Object.fromEntries(
    Object.entries(item.action.context).filter(([name]) => item.attachments.some((a) => a.field === name)),
  )
  const context = contextFor(kind, live.definition, form, base, kept, item.record.id)
  const revision = currentRevision(current)
  const done = await runtime.store.resolve(item.id, item.operation, (i, copy) => {
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
  })
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
// it. The restore is the engine's first step when it sends the item, and the change goes on top
// of the revision the restore gives; if the restore is refused (the person may not), the item
// says so and the person discards it. Never done by itself: only this choice brings a record back.
export async function restoreAndApply(runtime: Runtime, item: OutboxItem, deleted: Deleted): Promise<boolean> {
  const done = await runtime.store.resolve(item.id, item.operation, (i) => {
    if (i.attention !== 'deleted' || kindOf(i) !== 'update') return undefined
    return {
      item: {
        ...requeued(i),
        restore: { revision: deleted.revision },
        // A restore makes the next revision, which the change is based on.
        record: { ...i.record, revision: deleted.revision + 1 },
      },
    }
  })
  if (done) await runtime.engine.run()
  return done
}
