import { useCallback } from 'react'
import type { Format, T } from '../i18n'

export type Item = { id: number; _revision?: number; _labels?: Record<string, string> } & Record<string, unknown>

export type FieldType = 'string' | 'number' | 'boolean' | 'date' | 'time' | 'choice' | 'barcode'

export const MAX_SHOWN_ROWS = 200

const CALENDAR_DATE = /^\d{4}-\d{2}-\d{2}$/

export function readItems(value: unknown): Item[] {
  if (!Array.isArray(value)) return []
  return value.filter(
    (i): i is Item =>
      typeof i === 'object' && i !== null && Number.isInteger((i as { id?: unknown }).id) && (i as Item).id >= 1,
  )
}

function labelOf(item: Item, field: string): string | undefined {
  const label = item._labels?.[field]
  return typeof label === 'string' ? label : undefined
}

function present(value: unknown): boolean {
  return value !== undefined && value !== null && value !== ''
}

export function fieldValueText(item: Item, field: string, type: FieldType | undefined, f: Format, t: T): string {
  const value = item[field]
  if (!present(value)) return ''
  switch (type) {
    case 'choice':
      return labelOf(item, field) ?? String(value)
    case 'date':
      return typeof value === 'string' && CALENDAR_DATE.test(value) ? f.calendarDate(value) : String(value)
    case 'number':
      return typeof value === 'number' ? f.number(value) : String(value)
    case 'boolean':
      return typeof value === 'boolean' ? t(value ? 'apps.table.yes' : 'apps.table.no') : String(value)
    default:
      return String(value)
  }
}

function compare(a: Item, b: Item, field: string, type: FieldType | undefined, collator: Intl.Collator): number {
  const x = a[field]
  const y = b[field]
  if (typeof x === 'number' && typeof y === 'number') return x - y
  if (typeof x === 'boolean' && typeof y === 'boolean') return Number(x) - Number(y)
  const left = type === 'choice' ? (labelOf(a, field) ?? String(x)) : String(x)
  const right = type === 'choice' ? (labelOf(b, field) ?? String(y)) : String(y)
  return type === 'date' || type === 'time' ? (left < right ? -1 : left > right ? 1 : 0) : collator.compare(left, right)
}

export function sortItems(
  items: readonly Item[],
  field: string | undefined,
  type: FieldType | undefined,
  direction: 'asc' | 'desc',
  locale: string,
): Item[] {
  if (field === undefined) return [...items].sort((a, b) => a.id - b.id)
  const collator = new Intl.Collator(locale, { numeric: true })
  const sign = direction === 'desc' ? -1 : 1
  return [...items].sort((a, b) => {
    const missingA = !present(a[field])
    const missingB = !present(b[field])
    if (missingA || missingB) return missingA === missingB ? a.id - b.id : missingA ? 1 : -1
    return sign * compare(a, b, field, type, collator) || a.id - b.id
  })
}

export type ActionDispatcher = { dispatchAction(action: unknown): Promise<void> }

export function useDispatch(context: ActionDispatcher): (name: string, eventContext?: Record<string, unknown>) => void {
  return useCallback(
    (name, eventContext) => {
      void context.dispatchAction({ event: eventContext ? { name, context: eventContext } : { name } })
    },
    [context],
  )
}
