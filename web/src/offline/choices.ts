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
  seenAs,
  seenOf,
  versionOf,
} from './store'
import { NOT_WRITER, newOperation } from './transport'

type Definition = Download['definition']

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

const changedElsewhere = () => new ApiError(409, 'this changed in another tab', { code: 'offline_item_changed' })

export const REVIEW_AGAIN = 'offline_review_again'
const reviewAgain = () => new ApiError(409, 'the record changed again', { code: REVIEW_AGAIN })

const notWriter = () => new ApiError(409, 'changes are being made in another tab', { code: NOT_WRITER })

const copyChanged = () => new ApiError(409, 'the downloaded copy changed', { code: 'offline_copy_changed' })

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

async function stillShown(runtime: Runtime, item: OutboxItem, seen: Seen): Promise<number | null> {
  if (!runtime.writer.holds()) throw refused(runtime, notWriter())
  const stored = await runtime.store.getItem(item.id)
  if (!stored || versionOf(stored) !== seen.version || stored.operation !== seen.operation) {
    throw refused(runtime, changedElsewhere())
  }
  return runtime.store.downloadGeneration(item)
}

function refused(runtime: Runtime, e: ApiError): ApiError {
  runtime.store.refresh()
  return e
}

export async function discard(runtime: Runtime, item: OutboxItem, seen: SeenAs): Promise<void> {
  if (!runtime.writer.holds()) throw refused(runtime, notWriter())
  try {
    await chosen(() => runtime.store.discardItem(item.id, seen))
  } catch (e) {
    if (e instanceof ApiError && e.code === 'offline_item_changed') {
      const now = await runtime.store.getItem(item.id).catch(() => undefined)
      if (now && (now.state === 'syncing' || now.state === 'synced'))
        throw new ApiError(409, 'the record is being sent', { code: 'offline_item_sending' })
    }
    throw e
  }
  await runtime.engine.run()
}

async function pressed(runtime: Runtime, go: () => Promise<void>): Promise<void> {
  if (!runtime.writer.holds()) throw refused(runtime, notWriter())
  await chosen(go)
}

export const retryItem = (runtime: Runtime, item: OutboxItem, seen: SeenAs = seenAs(item)) =>
  pressed(runtime, () => runtime.engine.retry(item.id, seen))

export const holdItem = (runtime: Runtime, item: OutboxItem, seen: SeenAs = seenAs(item)) =>
  pressed(runtime, () => runtime.engine.hold(item.id, seen))

export const releaseItem = (runtime: Runtime, item: OutboxItem, seen: SeenAs = seenAs(item)) =>
  pressed(runtime, () => runtime.engine.release(item.id, seen))

export type ServerRecord = { current: Record<string, unknown> | null; deleted: boolean }

export async function readCurrent(runtime: Runtime, item: OutboxItem): Promise<ServerRecord> {
  try {
    const answer = await api<{
      record: { id: number; revision: number; data: Record<string, unknown>; deleted_at?: string | null }
    }>(`${item.base}/records/${item.record.id}/history`, { headers: runtime.engine.ownerHeaders() })
    assertSignedIn(runtime.store.user)
    const r = answer.record
    if (r.deleted_at) return { current: null, deleted: true }
    return { current: { id: r.id, _revision: r.revision, ...r.data }, deleted: false }
  } catch (e) {
    assertSignedIn(runtime.store.user)
    if (e instanceof ApiError && (e.status === 403 || e.status === 404)) return { current: null, deleted: false }
    throw e
  }
}

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

export async function refreshDefinition(runtime: Runtime, item: OutboxItem): Promise<Download> {
  return downloadApp(runtime.store, item)
}

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

export type Deleted = { revision: number; at: string | null; by: string | null }

export async function findDeleted(runtime: Runtime, item: OutboxItem): Promise<Deleted | null> {
  const list = await api<{
    records: { id: number; revision: number; deleted_at: string | null; deleted_by: { name?: string } | null }[]
  }>(`${item.base}/records/deleted`, { headers: runtime.engine.ownerHeaders() })
  assertSignedIn(runtime.store.user)
  const found = list.records.find((r) => r.id === item.record.id)
  return found ? { revision: found.revision, at: found.deleted_at, by: found.deleted_by?.name ?? null } : null
}

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
          record: { ...i.record, revision: now.revision + 1 },
        },
      }
    }),
  )
  if (done) await runtime.engine.run()
  return done
}
