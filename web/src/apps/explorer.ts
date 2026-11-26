// The records explorer's query: what it asks the server for (…/records/query), and the small rules
// the controls follow. The server decides what each person reads and what matches; this only
// builds the request. Its order, and what a total is, are documented in docs/api.md.

import type { AppField, FieldType } from '../api'

export type Tab = 'live' | 'deleted'
export type Dir = 'asc' | 'desc'
export type Sort = { by: string; dir: Dir }
export type Filter = { field: string; op: string; value: string }

// Records in a page.
export const PAGE_SIZE = 25

// The sort keys that aren't fields.
export const UPDATED = '_updated'
export const DELETED = '_deleted'

// The order a tab opens in: the most recently changed first (or deleted, for the deleted ones).
export const defaultSort = (tab: Tab): Sort => ({ by: tab === 'deleted' ? DELETED : UPDATED, dir: 'desc' })

// The direction a sort starts in when a column is chosen: the times newest first, the rest up.
export const firstDir = (by: string): Dir => (by === UPDATED || by === DELETED ? 'desc' : 'asc')

const ORDERED: FieldType[] = ['number', 'date', 'time', 'string']

// The types the server can neither filter nor sort by (internal/apps/query.go): a location or a
// signature is evidence, not something to compare, and a photo is no more an order or a value to
// match. It answers a filter or a sort on one with an error.
const NOT_COMPARED: FieldType[] = ['location', 'signature', 'attachment']

// The fields a query can filter by: the ones whose values it can compare.
export const filterable = (fields: AppField[]) => fields.filter((f) => !NOT_COMPARED.includes(f.type))

// The fields a query can sort by: the same ones.
export const sortable = filterable

// The operators the server takes for a field of this type: none for a type it can't filter by.
export function opsFor(type: FieldType): string[] {
  if (NOT_COMPARED.includes(type)) return []
  if (type === 'boolean' || type === 'choice') return ['eq', 'ne']
  const ops = ['eq', 'ne']
  if (ORDERED.includes(type)) ops.push('lt', 'lte', 'gt', 'gte')
  if (type === 'string' || type === 'barcode') ops.push('contains')
  return ops
}

export type Query = {
  tab: Tab
  filters: Filter[]
  search: string
  sort: Sort
  cursor: string
  // A number field to total: its sum, average, smallest and largest over every record that matches.
  totals: string | null
}

// The query's parameters, in a stable order so the same query is the same string.
export function queryString(q: Query): string {
  const p = new URLSearchParams()
  if (q.tab === 'deleted') p.set('state', 'deleted')
  for (const f of q.filters) p.append('filter', `${f.field}:${f.op}:${f.value}`)
  if (q.search.trim()) p.set('q', q.search.trim())
  p.set('sort', q.sort.by)
  p.set('dir', q.sort.dir)
  p.set('limit', String(PAGE_SIZE))
  if (q.cursor) p.set('cursor', q.cursor)
  if (q.totals) for (const kind of ['sum', 'avg', 'min', 'max']) p.append('aggregate', `${kind}:${q.totals}`)
  return p.toString()
}

// A stored value as the text a form input holds.
export function toText(v: unknown): string {
  if (v === undefined || v === null) return ''
  return typeof v === 'string' ? v : String(v)
}

export const numberFields = (fields: AppField[]) => fields.filter((f) => f.type === 'number')

// The action of an app that changes a record, or removes one, if it has one: the explorer edits
// through the app's own actions, as its screen does. An ordinary edit prefers an action that sets
// nothing going (no effects), so that changing a field never runs what a button does, such as asking
// for a payment link; an app with only actions that have effects is edited through the first.
export function actionOf(
  actions: Record<string, { kind: string; effects?: unknown[] | null }> | undefined,
  kind: 'update' | 'delete',
) {
  const all = Object.entries(actions ?? {}).filter(([, a]) => a.kind === kind)
  return (all.find(([, a]) => !a.effects?.length) ?? all[0])?.[0]
}
