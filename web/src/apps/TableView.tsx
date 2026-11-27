import { createComponentImplementation } from '@a2ui/react/v0_9'
import { CommonSchemas } from '@a2ui/web_core/v0_9'
import { useMemo, useState } from 'react'
import { z } from 'zod'
import { useFormat, useT } from '../i18n'
import { Button } from '../ui/Button'
import { type Column, Table } from '../ui/Table'
import {
  type ActionDispatcher,
  type FieldType,
  fieldValueText,
  type Item,
  MAX_SHOWN_ROWS,
  readItems,
  sortItems,
  useDispatch,
} from './collectionView'
import './TableView.css'

const FIELD_TYPES = ['string', 'number', 'boolean', 'date', 'time', 'choice', 'barcode'] as const

export const TableApi = {
  name: 'Table',
  schema: z
    .object({
      items: CommonSchemas.DynamicValue,
      columns: z.array(
        z
          .object({
            field: z.string(),
            label: CommonSchemas.DynamicString,
            type: z.enum(FIELD_TYPES).optional(),
          })
          .strict(),
      ),
      label: CommonSchemas.DynamicString,
      open: z.string().optional(),
      sort: z.string().optional(),
      dir: z.enum(['asc', 'desc']).optional(),
      accessibility: CommonSchemas.AccessibilityAttributes.optional(),
      weight: z.number().optional(),
    })
    .strict(),
}

type TableColumn = { field: string; label: string; type?: FieldType }

type SortState = { field: string | undefined; dir: 'asc' | 'desc' }

function TableView({
  items,
  columns,
  caption,
  open,
  initial,
  context,
}: {
  items: unknown
  columns: readonly TableColumn[]
  caption: string
  open: string | undefined
  initial: SortState
  context: ActionDispatcher
}) {
  const t = useT()
  const f = useFormat()
  const dispatch = useDispatch(context)
  const [sort, setSort] = useState(initial)
  const rows = useMemo(() => readItems(items), [items])
  const typeOf = (field: string) => columns.find((c) => c.field === field)?.type
  const ordered = useMemo(
    () => sortItems(rows, sort.field, columns.find((c) => c.field === sort.field)?.type, sort.dir, f.locale),
    [rows, sort, columns, f.locale],
  )
  const shown = ordered.slice(0, MAX_SHOWN_ROWS)
  const text = (item: Item, c: TableColumn) => fieldValueText(item, c.field, typeOf(c.field), f, t)
  const sorted = (c: TableColumn) => (sort.field === c.field ? sort.dir : undefined)
  const heads: Column<Item>[] = columns.map((c, i) => ({
    key: String(i),
    header: c.label,
    numeric: c.type === 'number',
    rowHeader: open !== undefined && i === 0,
    sort: {
      direction: sorted(c) === 'asc' ? 'ascending' : sorted(c) === 'desc' ? 'descending' : 'none',
      onSort: () => setSort({ field: c.field, dir: sorted(c) === 'asc' ? 'desc' : 'asc' }),
    },
    cell: (item) => {
      const shownText = text(item, c)
      if (open === undefined || i !== 0) return shownText
      const title = shownText || String(item.id)
      return (
        <Button
          variant="ghost"
          size="sm"
          aria-label={t('apps.table.open', { title })}
          onClick={() => dispatch(open, { id: item.id })}
        >
          {title}
        </Button>
      )
    },
  }))
  return (
    <div className="ui-app-table">
      <Table caption={caption} columns={heads} rows={shown} rowKey={(item) => item.id} />
      {rows.length > shown.length && (
        <p className="ui-app-table-more" role="status">
          {t('apps.table.more', { shown: f.number(shown.length), total: f.number(rows.length) })}
        </p>
      )}
    </div>
  )
}

export const TableComponent = createComponentImplementation(TableApi, ({ props, context }) => (
  <TableView
    items={props.items}
    columns={props.columns}
    caption={props.label}
    open={props.open}
    initial={{ field: props.sort, dir: props.dir ?? 'asc' }}
    context={context}
  />
))
