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
  format?: string
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
  // A scanner's text is taken as it was read: a line break or a tab around a code is no padding to
  // trim but a character that isn't part of one (the server refuses it too).
  if (f.type === 'barcode' && typeof raw === 'string' && raw.trim() !== '' && CONTROL.test(raw)) {
    throw refused('app_field_not_barcode', { field: f.name, format: f.format ?? '' })
  }
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
    case 'barcode':
      return parseBarcode(f, v)
    case 'location':
      return parseLocation(f, v)
    case 'signature':
      return parseSignature(f, v)
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

// The fields of an app's collection that hold pictures made on this device: photos and signatures.
// Both are sent as the queue's attachments: uploaded first, then named by their server ids.
export function attachmentFields(def: Definition): string[] {
  return def.collection.fields.filter((f) => f.type === 'attachment' || f.type === 'signature').map((f) => f.name)
}

// The attachment ids a value of such a field holds: a list of photos, or the one a signature has.
export function attachmentIdsOf(value: unknown): string[] {
  if (typeof value === 'string') return [value]
  return Array.isArray(value) ? value.filter((x): x is string => typeof x === 'string') : []
}

// ---- the capture fields: mirrors of the server's checks (internal/apps/capture.go) ----------------------

const CONTROL = /\p{Cc}/u
const ATTACHMENT_ID = /^[0-9a-f]{32}$/

// The formats whose text is a number the server checks: its length, and whether the last digit is a
// check digit (GS1: the weights 3 and 1 alternate from the digit before it).
const CHECKED: Record<string, { lengths: number[]; check: boolean }> = {
  ean_13: { lengths: [13], check: true },
  ean_8: { lengths: [8], check: true },
  upc_a: { lengths: [12], check: true },
  upc_e: { lengths: [6, 7, 8], check: false },
}

function checkDigitOk(digits: string): boolean {
  let sum = 0
  for (let i = digits.length - 2; i >= 0; i--) {
    const d = Number(digits[i])
    sum += (digits.length - 2 - i) % 2 === 0 ? d * 3 : d
  }
  return (10 - (sum % 10)) % 10 === Number(digits[digits.length - 1])
}

function parseBarcode(f: Field, v: unknown): string {
  const bad = () => refused('app_field_not_barcode', { field: f.name, format: f.format ?? '' })
  if (typeof v !== 'string') throw bad()
  const max = f.max_length ?? 1000
  if ([...v].length > max) throw refused('app_field_too_long', { field: f.name, max })
  const rule = f.format ? CHECKED[f.format] : undefined
  if (rule && (!rule.lengths.includes(v.length) || !/^\d+$/.test(v) || (rule.check && !checkDigitOk(v)))) throw bad()
  return v
}

// A location, as the server stores it: a position within the globe, how sure the device was, when it
// said so (RFC 3339, from 2000 on, and not later than the clock by more than five minutes) and where it
// came from. An object with no keys is no value.
const LOCATION_KEYS = ['lat', 'lon', 'accuracy_m', 'captured_at', 'source']
const MAX_ACCURACY = 1_000_000
const MAX_CLOCK_SKEW = 5 * 60 * 1000
const RFC3339 = /^(\d{4})-(\d{2})-(\d{2})T([01]\d|2[0-3]):([0-5]\d):([0-5]\d)(\.\d+)?(Z|[+-]([01]\d|2[0-3]):[0-5]\d)$/

function parseLocation(f: Field, v: unknown): Record<string, unknown> | undefined {
  const bad = () => refused('app_field_not_location', { field: f.name })
  if (!v || typeof v !== 'object' || Array.isArray(v)) throw bad()
  const obj = v as Record<string, unknown>
  const keys = Object.keys(obj)
  if (keys.length === 0) return undefined
  if (keys.some((k) => !LOCATION_KEYS.includes(k))) throw bad()
  const { lat, lon, accuracy_m: accuracy, captured_at: at, source } = obj
  const within = (n: unknown, limit: number): n is number =>
    typeof n === 'number' && Number.isFinite(n) && Math.abs(n) <= limit
  if (!within(lat, 90) || !within(lon, 180)) throw bad()
  if (source !== 'device' && source !== 'manual') throw bad()
  const out: Record<string, unknown> = { lat, lon, source }
  if (accuracy !== undefined && accuracy !== null) {
    if (!within(accuracy, MAX_ACCURACY) || accuracy < 0) throw bad()
    out.accuracy_m = accuracy
  } else if (source === 'device') {
    throw bad()
  }
  const time = typeof at === 'string' ? RFC3339.exec(at) : null
  if (!time) throw bad()
  const [year, month, day, hour, minute, second] = time.slice(1, 7).map(Number)
  const stamp = Date.UTC(year, month - 1, day, hour, minute, second)
  // A date that isn't one (the 30th of February) is no time.
  const check = new Date(stamp)
  if (check.getUTCFullYear() !== year || check.getUTCMonth() !== month - 1 || check.getUTCDate() !== day) throw bad()
  const instant = Date.parse(at as string)
  if (!Number.isFinite(instant) || year < 2000) throw bad()
  if (instant > Date.now() + MAX_CLOCK_SKEW) throw refused('app_field_location_future', { field: f.name })
  out.captured_at = new Date(instant).toISOString()
  return out
}

// A signature is one attachment id: the id of the picture staged on this device (which the queue
// uploads and replaces with the server's), as text or as a list of one.
function parseSignature(f: Field, v: unknown): string | undefined {
  const bad = () => refused('app_field_not_signature', { field: f.name })
  let id = v
  if (Array.isArray(v)) {
    if (v.length === 0) return undefined
    if (v.length !== 1) throw bad()
    id = v[0]
  }
  if (typeof id !== 'string' || !ATTACHMENT_ID.test(id)) throw bad()
  return id
}

// Items as the data model holds them: id, then the values.
type Item = Record<string, unknown>

// The records an app shows offline: the downloaded ones, then the ones queued on this device.
export function itemsOf(download: Download, queue: OutboxItem[]): Item[] {
  const base = dataModelOf(download.messages)
  const downloaded = (Array.isArray(base?.items) ? (base.items as Item[]) : []).slice()
  // A record this device sent is among the downloaded ones once a download was taken after the
  // server had it (the download names the operation that made each of the person's records):
  // it is the server's, shown once, and never again as the device's own.
  const local = queue
    .filter((q) => q.action.name in download.definition.actions)
    .filter((q) => !(q.operation in (download.operations ?? {})))
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
function summarize(s: { kind: string; field?: string; where?: unknown; month_of?: string }, rows: Item[]): string {
  const month = new Date().toISOString().slice(0, 7)
  const dated = s.month_of
    ? rows.filter(
        (r) =>
          typeof r[s.month_of as string] === 'string' && (r[s.month_of as string] as string).startsWith(`${month}-`),
      )
    : rows
  const kept = s.where ? dated.filter((r) => holds(s.where as Rule, r)) : dated
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
