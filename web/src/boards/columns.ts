import type { Card } from '../api'
import type { Key } from '../i18n'

export const COLUMN_LABELS: Record<Card['column'], Key> = {
  backlog: 'boards.columns.backlog',
  todo: 'boards.columns.todo',
  doing: 'boards.columns.doing',
  review: 'boards.columns.review',
  done: 'boards.columns.done',
  archived: 'boards.columns.archived',
}

export const COLUMNS = (['backlog', 'todo', 'doing', 'review', 'done'] as const).map((id) => ({
  id,
  label: COLUMN_LABELS[id],
}))

export type Column = (typeof COLUMNS)[number]
