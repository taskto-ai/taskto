// A table of data: column headers, numbers aligned to the end in tabular figures, and, when it is
// wider than its place (a phone), it scrolls sideways in its own box, which can then be reached
// and scrolled with the keyboard. Put it in a Panel for an edge.

import { type ReactNode, useEffect, useRef, useState } from 'react'
import { VisuallyHidden } from './VisuallyHidden'
import './Table.css'
import './VisuallyHidden.css'

export interface Column<T> {
  key: string
  header: ReactNode
  // Numbers: aligned to the end, in tabular figures.
  numeric?: boolean
  // The header is read by a screen reader but not shown (a column of actions).
  hiddenHeader?: boolean
  // The column that names each row (a person, a plan): its cells are the rows' headers, so a screen
  // reader says which row a cell further along is in.
  rowHeader?: boolean
  cell: (row: T) => ReactNode
}

export interface TableProps<T> {
  // What the table is: its caption, read by a screen reader (and shown when `showCaption`).
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
    // A box that scrolls is a region with a name and a tab stop, so it can be scrolled without a
    // pointer; one that doesn't is neither.
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
              <th key={c.key} scope="col" className="ui-table-head" data-numeric={c.numeric || undefined}>
                {c.hiddenHeader ? <VisuallyHidden>{c.header}</VisuallyHidden> : c.header}
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

// Whether an element's content is wider than it, kept up to date as either changes size.
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
