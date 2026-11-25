import type { Card } from '../api'

// The dot of a column's status, in the column's hue (backlog is a hollow ring). The column's name
// is always written beside it, so it says nothing to a screen reader.
export function StatusDot({ column }: { column: Card['column'] }) {
  return <span className="ui-board-dot" data-column={column} aria-hidden="true" />
}
