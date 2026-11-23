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
import type { Download, OutboxItem } from './store'

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
  for (const f of fields) {
    const missing = values[f.name] === undefined
    if (missing && f.required) throw refused('app_field_required', { field: f.name })
    if (missing && f.required_when && holds(f.required_when, values)) {
      throw refused('app_field_required_when', { field: f.name })
    }
  }
  return values
}

function parseField(f: Field, raw: unknown): unknown {
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

// The records an app shows offline: the downloaded ones, then the ones queued on this device.
export function itemsOf(download: Download, queue: OutboxItem[]): Item[] {
  const base = dataModelOf(download.messages)
  const downloaded = (Array.isArray(base?.items) ? (base.items as Item[]) : []).slice()
  const local = queue
    .filter((q) => q.action.name in download.definition.actions)
    .map((q) => {
      // What the server will store: the values the context gives, parsed. (The queue holds the
      // context as sent; a context that parsed when it was queued parses again.)
      let values: Record<string, unknown> = {}
      try {
        values = parseInsert(download.definition, q.action.context)
      } catch {
        values = {}
      }
      return { id: q.record.id, ...values }
    })
  return [...downloaded, ...local]
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

// What the screen is sent after a record was queued: as the server answers an insert. An app with
// one screen has its whole data model drawn again (the form emptied); an app with screens has the
// records, the summaries and the form replaced, and goes on to the screen the action names.
export function answerInsert(download: Download, queue: OutboxItem[], actionName: string): unknown[] {
  const surface = surfaceOf(download.messages)
  const items = itemsOf(download, queue)
  const def = download.definition
  if (!def.screens?.length) return viewOf(download, queue).filter((m) => (m as Message).updateDataModel)
  const out: Message[] = [dataMessage(surface, '/items', items)]
  for (const [name, s] of Object.entries(def.summaries ?? {}).sort(([a], [b]) => (a < b ? -1 : 1))) {
    out.push(dataMessage(surface, `/${name}`, summarize(s, items)))
  }
  out.push(dataMessage(surface, '/draft', {}))
  const then = def.actions[actionName]?.then
  if (then) out.push(dataMessage(surface, '/screen', then))
  return out
}

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
