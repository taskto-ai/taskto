import type { Card } from '../api'

export function StatusDot({ column }: { column: Card['column'] }) {
  return <span className="ui-board-dot" data-column={column} aria-hidden="true" />
}
