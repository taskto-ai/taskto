import { ApiError, type OfflineApp } from '../api'
import { holds, type Rule } from '../apps/rules'
import { type ChangeKind, type Download, kindOf, type OutboxItem } from './store'

type Definition = OfflineApp['definition']
type Field = Definition['collection']['fields'][number] & {
  required_when?: Rule
  max_files?: number
  max_bytes?: number
  format?: string
}

export type Message = Record<string, unknown>

function refused(code: string, params?: Record<string, unknown>): ApiError {
  return new ApiError(422, code, { error: code, code, ...(params ? { params } : {}) })
}

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

export function requireFields(def: Definition, values: Record<string, unknown>): void {
  for (const f of def.collection.fields as Field[]) {
    const missing = values[f.name] === undefined
    if (missing && f.required) throw refused('app_field_required', { field: f.name })
    if (missing && f.required_when && holds(f.required_when, values)) {
      throw refused('app_field_required_when', { field: f.name })
    }
  }
}

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

export function fieldsOf(item: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const [k, v] of Object.entries(item)) if (k !== 'id' && !k.startsWith('_')) out[k] = v
  return out
}

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

export function attachmentFields(def: Definition): string[] {
  return def.collection.fields.filter((f) => f.type === 'attachment' || f.type === 'signature').map((f) => f.name)
}

export function attachmentIdsOf(value: unknown): string[] {
  if (typeof value === 'string') return [value]
  return Array.isArray(value) ? value.filter((x): x is string => typeof x === 'string') : []
}

const CONTROL = /\p{Cc}/u
const ATTACHMENT_ID = /^[0-9a-f]{32}$/

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
  const check = new Date(stamp)
  if (check.getUTCFullYear() !== year || check.getUTCMonth() !== month - 1 || check.getUTCDate() !== day) throw bad()
  const instant = Date.parse(at as string)
  if (!Number.isFinite(instant) || year < 2000) throw bad()
  if (instant > Date.now() + MAX_CLOCK_SKEW) throw refused('app_field_location_future', { field: f.name })
  out.captured_at = new Date(instant).toISOString()
  return out
}

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

type Item = Record<string, unknown>

export function itemsOf(download: Download, queue: OutboxItem[]): Item[] {
  const base = dataModelOf(download.messages)
  let items = (Array.isArray(base?.items) ? (base.items as Item[]) : []).slice()
  const created: Item[] = []
  for (const q of [...queue].sort((a, b) => a.seq - b.seq)) {
    if (!(q.action.name in download.definition.actions)) continue
    if (q.operation in (download.operations ?? {})) continue
    const kind = kindOf(q)
    if (kind === 'insert') {
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
  return [...items, ...created].map((i) => withLabels(download.definition, i))
}

function withLabels(def: Definition, item: Item): Item {
  const { _labels: _was, ...rest } = item
  const labels: Record<string, string> = {}
  for (const f of def.collection.fields) {
    const v = item[f.name]
    if (f.type === 'choice' && typeof v === 'string') {
      labels[f.name] = f.option_labels && Object.hasOwn(f.option_labels, v) ? f.option_labels[v] : v
    }
  }
  return Object.keys(labels).length > 0 ? { ...rest, _labels: labels } : rest
}

function overlay(def: Definition, items: Item[], q: OutboxItem, kind: ChangeKind): Item[] {
  const at = items.findIndex((i) => i.id === q.record.id)
  if (at < 0) return items
  const held = typeof items[at]._revision === 'number' ? (items[at]._revision as number) : undefined
  const based = held === undefined || q.record.revision === null || held === q.record.revision
  const replace = (next: Item) => items.map((it, i) => (i === at ? next : it))
  if (q.state === 'synced') {
    const ackRevision = typeof q.ack?._revision === 'number' ? q.ack._revision : undefined
    if (kind === 'update' && q.ack && ackRevision !== undefined) {
      return held === undefined || ackRevision >= held ? replace({ ...q.ack, id: q.record.id }) : items
    }
    if (!based) return items
  } else if (!based) {
    return replace({ ...items[at], _pending: 'review' })
  }
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
    delete next._revision
  } else {
    next._pending = 'update'
    if (q.record.revision !== null) next._revision = q.record.revision
  }
  return replace(next)
}

export function downloadedItem(download: Download, id: number): Item | undefined {
  const base = dataModelOf(download.messages)
  return (Array.isArray(base?.items) ? (base.items as Item[]) : []).find((i) => i.id === id)
}

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

function dataModelOf(messages: unknown[]): Record<string, unknown> | undefined {
  for (const m of messages as Message[]) {
    const update = m.updateDataModel as { path?: string; value?: unknown } | undefined
    if (update && update.path === '/' && update.value && typeof update.value === 'object') {
      return update.value as Record<string, unknown>
    }
  }
  return undefined
}

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
