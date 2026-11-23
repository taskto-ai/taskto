// An app used without the server: what a downloaded release shows, and what an insert made
// offline does to it, worked out here from the download and the queue.
//
// This never decides anything. The server checks every record again when it comes in (its
// answer is the only one that says a record was accepted), so what is checked here is only what
// saves the person a wait: a required field, a number that isn't one, a photo's limits. The
// counts and sums the app shows are computed over the downloaded records and the records queued
// on this device, and the page says so (OfflineBanner): they are never the server's totals.

import { ApiError, type OfflineApp } from '../api'
import { holds, type Rule } from '../apps/rules'
import { type ChangeKind, type Download, kindOf, type OutboxItem } from './store'

type Definition = OfflineApp['definition']
type Field = Definition['collection']['fields'][number] & {
  required_when?: Rule
  max_files?: number
  max_bytes?: number
}

export type Message = Record<string, unknown>

// A refusal made on the device, shaped as the server's so the page says it the same way
// (errors.<code> in the locale files).
function refused(code: string, params?: Record<string, unknown>): ApiError {
  return new ApiError(422, code, { error: code, code, ...(params ? { params } : {}) })
}

// The record the context would make, each value as the server stores it, or the first thing wrong
// with it. A mirror of the server's Field.parse for the insert's fields.
export function parseInsert(def: Definition, context: Record<string, unknown>): Record<string, unknown> {
  const fields = def.collection.fields as Field[]
  const byName = new Map(fields.map((f) => [f.name, f]))
  const values: Record<string, unknown> = {}
  for (const name of Object.keys(context)) {
    if (!byName.has(name)) throw refused('bad_request', { field: name })
  }
  for (const f of fields) {
    const v = parseField(f, context[f.name])
    if (v !== undefined) values[f.name] = v
  }
  requireFields(def, values)
  return values
}

// The first required field a record lacks: always required, or required when its rule holds.
export function requireFields(def: Definition, values: Record<string, unknown>): void {
  for (const f of def.collection.fields as Field[]) {
    const missing = values[f.name] === undefined
    if (missing && f.required) throw refused('app_field_required', { field: f.name })
    if (missing && f.required_when && holds(f.required_when, values)) {
      throw refused('app_field_required_when', { field: f.name })
    }
  }
}

// What an update sets, each value as the server stores it, and null for a field it clears. A
// mirror of the server's Plan for an update: only the fields the context names (and not its id),
// each of which must be one the collection declares.
export function parseUpdate(def: Definition, context: Record<string, unknown>): Record<string, unknown> {
  const byName = new Map((def.collection.fields as Field[]).map((f) => [f.name, f]))
  const changes: Record<string, unknown> = {}
  for (const [name, raw] of Object.entries(context)) {
    if (name === 'id') continue
    const f = byName.get(name)
    if (!f) throw refused('bad_request', { field: name })
    changes[name] = parseField(f, raw) ?? null
  }
  return changes
}

// The record's own fields: an item without its id and the keys the device adds (`_revision`,
// `_pending`).
export function fieldsOf(item: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const [k, v] of Object.entries(item)) if (k !== 'id' && !k.startsWith('_')) out[k] = v
  return out
}

// Applies an update's changes to a record's fields and checks the result against the collection,
// as the server does before it writes: a required field must not end up missing.
export function applyUpdate(
  def: Definition,
  fields: Record<string, unknown>,
  changes: Record<string, unknown>,
): Record<string, unknown> {
  const merged = { ...fields }
  for (const [k, v] of Object.entries(changes)) {
    if (v === null) delete merged[k]
    else merged[k] = v
  }
  requireFields(def, merged)
  return merged
}

export function parseField(f: Field, raw: unknown): unknown {
  let v = raw
  if (Array.isArray(v) && f.type === 'choice') {
    if (v.length === 0) return undefined
    if (v.length > 1) throw refused('app_field_not_option', { field: f.name })
    v = v[0]
  }
  if (typeof v === 'string') {
    v = v.trim()
    if (v === '') return undefined
  }
  if (v === undefined || v === null) return undefined
  switch (f.type) {
    case 'string': {
      if (typeof v !== 'string') throw refused('bad_request', { field: f.name })
      const max = f.max_length ?? 1000
      if ([...v].length > max) throw refused('app_field_too_long', { field: f.name, max })
      return v
    }
    case 'number': {
      const n = typeof v === 'number' ? v : typeof v === 'string' ? Number(v) : Number.NaN
      if (!Number.isFinite(n)) throw refused('app_field_not_number', { field: f.name })
      if (f.min !== undefined && n < f.min) throw refused('app_field_too_small', { field: f.name, min: f.min })
      if (f.max !== undefined && n > f.max) throw refused('app_field_too_large', { field: f.name, max: f.max })
      return n
    }
    case 'boolean':
      if (v === true || v === 'true') return true
      if (v === false || v === 'false') return false
      throw refused('bad_request', { field: f.name })
    case 'date':
      if (typeof v !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(v) || Number.isNaN(Date.parse(`${v}T00:00:00Z`))) {
        throw refused('app_field_not_date', { field: f.name })
      }
      return v
    case 'time':
      if (typeof v !== 'string' || !/^([01]\d|2[0-3]):[0-5]\d$/.test(v)) {
        throw refused('app_field_not_time', { field: f.name })
      }
      return v
    case 'choice':
      if (typeof v !== 'string' || (f.options && !f.options.includes(v))) {
        throw refused('app_field_not_option', { field: f.name })
      }
      return v
    case 'attachment': {
      const max = f.max_files ?? 1
      if (!Array.isArray(v) || v.length === 0) return undefined
      if (v.length > max || v.some((x) => typeof x !== 'string')) {
        throw refused('app_field_files', { field: f.name, max })
      }
      return v
    }
  }
  return undefined
}

// The fields of an app's collection that hold photos.
export function attachmentFields(def: Definition): string[] {
  return def.collection.fields.filter((f) => f.type === 'attachment').map((f) => f.name)
}

// Items as the data model holds them: id, then the values.
type Item = Record<string, unknown>

// The records an app shows offline: the downloaded ones with the changes waiting on this device
// applied, then the records queued here. A record the person changed is shown as they left it and
// says so (`_pending`); one they deleted is not shown. `_revision` stays the revision the change
// was based on, so a further change is based on it too; once the server acknowledged a change it
// is the revision the server gave (unknown, and so left out, if its answer didn't say).
export function itemsOf(download: Download, queue: OutboxItem[]): Item[] {
  const base = dataModelOf(download.messages)
  let items = (Array.isArray(base?.items) ? (base.items as Item[]) : []).slice()
  const created: Item[] = []
  for (const q of [...queue].sort((a, b) => a.seq - b.seq)) {
    if (!(q.action.name in download.definition.actions)) continue
    // A record this device sent is among the downloaded ones once a download was taken after the
    // server had it (the download names the operation that made each of the person's records):
    // it is the server's, shown once, and never again as the device's own.
    if (q.operation in (download.operations ?? {})) continue
    const kind = kindOf(q)
    if (kind === 'insert') {
      // What the server will store: the values the context gives, parsed. (The queue holds the
      // context as sent; a context that parsed when it was queued parses again.)
      let values: Record<string, unknown> = {}
      try {
        values = parseInsert(download.definition, q.action.context)
      } catch {
        values = {}
      }
      created.push({ id: q.record.id, ...values })
    } else {
      items = overlay(download.definition, items, q, kind)
    }
  }
  return [...items, ...created]
}

// One waiting change to a downloaded record, applied to what is shown.
function overlay(def: Definition, items: Item[], q: OutboxItem, kind: ChangeKind): Item[] {
  const at = items.findIndex((i) => i.id === q.record.id)
  if (at < 0) return items
  if (kind === 'delete') return items.filter((_, i) => i !== at)
  let changes: Record<string, unknown>
  try {
    changes = parseUpdate(def, q.action.context)
  } catch {
    return items
  }
  const next: Item = { ...items[at] }
  for (const [k, v] of Object.entries(changes)) {
    if (v === null) delete next[k]
    else next[k] = v
  }
  if (q.state === 'synced') {
    if (q.ackRevision !== undefined) next._revision = q.ackRevision
    else delete next._revision
  } else {
    next._pending = 'update'
    if (q.record.revision !== null) next._revision = q.record.revision
  }
  return items.map((it, i) => (i === at ? next : it))
}

// The downloaded record, as the server had it (before any change waiting here).
export function downloadedItem(download: Download, id: number): Item | undefined {
  const base = dataModelOf(download.messages)
  return (Array.isArray(base?.items) ? (base.items as Item[]) : []).find((i) => i.id === id)
}

// The download with one record replaced by the server's (an item), or, with null, removed: what a
// conflict's resolution makes the copy hold. The counts follow.
export function withRecord(download: Download, id: number, record: Item | null): Download {
  let found = false
  const messages = (download.messages as Message[]).map((m) => {
    const update = m.updateDataModel as { path?: string; value?: unknown } | undefined
    if (update?.path !== '/' || !update.value || typeof update.value !== 'object') return m
    const value = update.value as Record<string, unknown>
    const items = Array.isArray(value.items) ? (value.items as Item[]) : []
    found = items.some((i) => i.id === id)
    const next = record
      ? found
        ? items.map((i) => (i.id === id ? record : i))
        : [...items, record]
      : items.filter((i) => i.id !== id)
    return { ...m, updateDataModel: { ...update, value: { ...value, items: next } } }
  })
  const delta = record ? (found ? 0 : 1) : found ? -1 : 0
  return {
    ...download,
    messages,
    records: Math.max(0, download.records + delta),
    total: Math.max(0, download.total + delta),
  }
}

// The first updateDataModel at the root: the whole data model the download was taken with.
function dataModelOf(messages: unknown[]): Record<string, unknown> | undefined {
  for (const m of messages as Message[]) {
    const update = m.updateDataModel as { path?: string; value?: unknown } | undefined
    if (update && update.path === '/' && update.value && typeof update.value === 'object') {
      return update.value as Record<string, unknown>
    }
  }
  return undefined
}

// A summary over rows, as the server formats one: the count as a number, a sum with two decimals.
function summarize(s: { kind: string; field?: string; where?: unknown }, rows: Item[]): string {
  const kept = s.where ? rows.filter((r) => holds(s.where as Rule, r)) : rows
  if (s.kind === 'count') return String(kept.length)
  if (s.kind === 'sum') {
    let total = 0
    for (const r of kept) {
      const n = s.field ? r[s.field] : undefined
      if (typeof n === 'number') total += n
    }
    return total.toFixed(2)
  }
  return ''
}

function dataMessage(surface: string, path: string, value: unknown): Message {
  return { version: 'v0.9', updateDataModel: { surfaceId: surface, path, value } }
}

function surfaceOf(messages: unknown[]): string {
  for (const m of messages as Message[]) {
    const id = (m.createSurface as { surfaceId?: string } | undefined)?.surfaceId
    if (id) return id
  }
  return 'app'
}

// The messages that draw the downloaded app with what is queued on top: the download's own, with
// the data model's records and summaries replaced.
export function viewOf(download: Download, queue: OutboxItem[]): unknown[] {
  const surface = surfaceOf(download.messages)
  const items = itemsOf(download, queue)
  return (download.messages as Message[]).map((m) => {
    const update = m.updateDataModel as { path?: string; value?: unknown } | undefined
    if (update?.path !== '/' || !update.value || typeof update.value !== 'object') return m
    const value: Record<string, unknown> = { ...(update.value as Record<string, unknown>), items }
    for (const [name, s] of Object.entries(download.definition.summaries ?? {})) value[name] = summarize(s, items)
    return dataMessage(surface, '/', value)
  })
}

// What the screen is sent after a change was queued: as the server answers it. An app with one
// screen has its whole data model drawn again (the form emptied); an app with screens has the
// records, the summaries (and, for an insert, the form) replaced, the record a detail screen shows
// shown again after an update and emptied after a delete, and goes on to the screen the action
// names.
export function answerChange(
  download: Download,
  queue: OutboxItem[],
  actionName: string,
  kind: ChangeKind = 'insert',
  id?: number,
): unknown[] {
  const surface = surfaceOf(download.messages)
  const items = itemsOf(download, queue)
  const def = download.definition
  if (!def.screens?.length) return viewOf(download, queue).filter((m) => (m as Message).updateDataModel)
  const out: Message[] = [dataMessage(surface, '/items', items)]
  for (const [name, s] of Object.entries(def.summaries ?? {}).sort(([a], [b]) => (a < b ? -1 : 1))) {
    out.push(dataMessage(surface, `/${name}`, summarize(s, items)))
  }
  if (kind === 'insert') out.push(dataMessage(surface, '/draft', {}))
  if (kind === 'update') out.push(dataMessage(surface, '/record', items.find((i) => i.id === id) ?? {}))
  if (kind === 'delete') out.push(dataMessage(surface, '/record', {}))
  const then = def.actions[actionName]?.then
  if (then) out.push(dataMessage(surface, '/screen', then))
  return out
}

export const answerInsert = (download: Download, queue: OutboxItem[], actionName: string): unknown[] =>
  answerChange(download, queue, actionName, 'insert')

// A navigate action, which changes no record: the screen to show and, for one that shows a record,
// the record.
export function answerNavigate(
  download: Download,
  queue: OutboxItem[],
  actionName: string,
  context: Record<string, unknown>,
): unknown[] {
  const surface = surfaceOf(download.messages)
  const def = download.definition
  const target = def.actions[actionName]?.screen
  const screen = def.screens?.find((s) => s.name === target)
  if (!screen) throw refused('bad_request')
  let record: Item = {}
  if (screen.record) {
    const id = Number(context.id)
    const found = itemsOf(download, queue).find((i) => i.id === id)
    if (!found) throw new ApiError(422, 'row not found', { error: 'row not found', code: 'app_row_not_found' })
    record = found
  }
  return [dataMessage(surface, '/screen', screen.name), dataMessage(surface, '/record', record)]
}
