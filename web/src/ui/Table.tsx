import { type ReactNode, useEffect, useRef, useState } from 'react'
import { Icon } from './Icon'
import { VisuallyHidden } from './VisuallyHidden'
import './Table.css'
import './VisuallyHidden.css'

export interface Column<T> {
  key: string
  header: ReactNode
  numeric?: boolean
  hiddenHeader?: boolean
  rowHeader?: boolean
  cell: (row: T) => ReactNode
  sort?: ColumnSort
}

export interface ColumnSort {
  direction: 'ascending' | 'descending' | 'none'
  onSort: () => void
}

export interface TableProps<T> {
  caption: string
  showCaption?: boolean
  columns: readonly Column<T>[]
  rows: readonly T[]
  rowKey: (row: T) => string | number
}

export function Table<T>({ caption, showCaption = false, columns, rows, rowKey }: TableProps<T>) {
  const box = useRef<HTMLDivElement>(null)
  const scrolls = useOverflows(box)
  return (
    <div
      ref={box}
      className="ui-table-scroll"
      {...(scrolls ? { role: 'region', 'aria-label': caption, tabIndex: 0 } : {})}
    >
      <table className="ui-table">
        <caption className={showCaption ? 'ui-table-caption' : 'ui-visually-hidden'}>{caption}</caption>
        <thead className="ui-table-header">
          <tr className="ui-table-row">
            {columns.map((c) => (
              <th
                key={c.key}
                scope="col"
                className="ui-table-head"
                data-numeric={c.numeric || undefined}
                aria-sort={c.sort && c.sort.direction !== 'none' ? c.sort.direction : undefined}
              >
                {c.sort ? (
                  <SortButton sort={c.sort}>{c.header}</SortButton>
                ) : c.hiddenHeader ? (
                  <VisuallyHidden>{c.header}</VisuallyHidden>
                ) : (
                  c.header
                )}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="ui-table-body">
          {rows.map((row) => (
            <tr key={rowKey(row)} className="ui-table-row">
              {columns.map((c) =>
                c.rowHeader ? (
                  <th
                    key={c.key}
                    scope="row"
                    className="ui-table-cell ui-table-rowhead"
                    data-numeric={c.numeric || undefined}
                  >
                    {c.cell(row)}
                  </th>
                ) : (
                  <td key={c.key} className="ui-table-cell" data-numeric={c.numeric || undefined}>
                    {c.cell(row)}
                  </td>
                ),
              )}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function SortButton({ sort, children }: { sort: ColumnSort; children: ReactNode }) {
  const icon = sort.direction === 'ascending' ? 'chevron-up' : sort.direction === 'descending' ? 'chevron-down' : null
  return (
    <button
      type="button"
      className="ui-table-sort ui-control"
      data-sorted={icon ? true : undefined}
      onClick={sort.onSort}
    >
      <span>{children}</span>
      <Icon name={icon ?? 'chevron-updown'} size={14} />
    </button>
  )
}

function useOverflows(ref: React.RefObject<HTMLElement | null>): boolean {
  const [overflows, setOverflows] = useState(false)
  useEffect(() => {
    const el = ref.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const check = () => setOverflows(el.scrollWidth > el.clientWidth + 1)
    const observer = new ResizeObserver(check)
    observer.observe(el)
    if (el.firstElementChild) observer.observe(el.firstElementChild)
    check()
    return () => observer.disconnect()
  }, [ref])
  return overflows
}
