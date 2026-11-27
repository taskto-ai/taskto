import type { AppField, FieldType } from '../api'

export type Tab = 'live' | 'deleted'
export type Dir = 'asc' | 'desc'
export type Sort = { by: string; dir: Dir }
export type Filter = { field: string; op: string; value: string }

export const PAGE_SIZE = 25

export const UPDATED = '_updated'
export const DELETED = '_deleted'

export const defaultSort = (tab: Tab): Sort => ({ by: tab === 'deleted' ? DELETED : UPDATED, dir: 'desc' })

export const firstDir = (by: string): Dir => (by === UPDATED || by === DELETED ? 'desc' : 'asc')

const ORDERED: FieldType[] = ['number', 'date', 'time', 'string']

const NOT_COMPARED: FieldType[] = ['location', 'signature', 'attachment']

export const filterable = (fields: AppField[]) => fields.filter((f) => !NOT_COMPARED.includes(f.type))

export const sortable = filterable

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
  totals: string | null
}

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

export function toText(v: unknown): string {
  if (v === undefined || v === null) return ''
  return typeof v === 'string' ? v : String(v)
}

export const numberFields = (fields: AppField[]) => fields.filter((f) => f.type === 'number')

export function actionOf(
  actions: Record<string, { kind: string; effects?: unknown[] | null }> | undefined,
  kind: 'update' | 'delete',
) {
  const all = Object.entries(actions ?? {}).filter(([, a]) => a.kind === kind)
  return (all.find(([, a]) => !a.effects?.length) ?? all[0])?.[0]
}
