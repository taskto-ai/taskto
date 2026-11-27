import type { DragEvent } from 'react'
import type { Card } from '../api'
import { type Format, type T, useT } from '../i18n'
import { AgentDot } from '../ui/AgentDot'
import { Avatar } from '../ui/Avatar'
import { Badge } from '../ui/Badge'
import { Icon } from '../ui/Icon'
import { Label } from '../ui/Label'
import { Menu, MenuButton, MenuItem, MenuList, MenuSeparator } from '../ui/Menu'
import { COLUMNS } from './columns'
import { type DueState, dueState } from './due'
import { dropPosition } from './move'
import { assigneeLabel } from './text'

export function BoardCard({
  card: c,
  all,
  shownInColumn,
  canWrite,
  current,
  dragged,
  today,
  f,
  onDragStart,
  onDragEnd,
  onDrop,
  onOpen,
  onMove,
}: {
  card: Card
  all: Card[]
  shownInColumn: Card[]
  canWrite: boolean
  current: boolean
  dragged: boolean
  today: string
  f: Format
  onDragStart: (e: DragEvent) => void
  onDragEnd: () => void
  onDrop: (e: DragEvent) => void
  onOpen: () => void
  onMove: (column: Card['column'], before?: Card) => void
}) {
  const t = useT()
  const due = dueState(c, today)
  const reason = c.blocked_reason?.trim()
  const meta = due !== null || c.labels.length > 0 || c.blocked || c.assignee !== null
  const details = [
    ...(c.blocked ? [t('boards.nameBlocked')] : []),
    ...(due && c.due ? [dueName(t, due, c.due, today, f)] : []),
  ]
  const name = details.length > 0 ? t('boards.cardName', { title: c.title, details: f.list(details) }) : c.title
  return (
    <li
      className="ui-board-card"
      draggable={canWrite}
      data-dragged={dragged || undefined}
      data-blocked={c.blocked || undefined}
      data-current={current || undefined}
      data-movable={canWrite || undefined}
      onDragStart={(e) => {
        if (!canWrite) return
        onDragStart(e)
      }}
      onDragEnd={onDragEnd}
      onDrop={onDrop}
    >
      <button
        type="button"
        className="ui-board-card-open"
        data-card-open={c.id}
        aria-current={current || undefined}
        aria-label={name}
        onClick={onOpen}
      >
        <span className="ui-board-card-title">{c.title}</span>
        {meta && (
          <span className="ui-board-card-meta">
            {c.blocked && (
              <Badge tone="danger" icon="block">
                {t('boards.blocked')}
              </Badge>
            )}
            {c.blocked && reason && <span className="ui-board-card-reason">{reason}</span>}
            {due && c.due && <DueBadge state={due} text={dueWords(t, due, c.due, today, f)} />}
            {c.labels.map((l) => (
              <Label key={l} name={l} />
            ))}
            {c.assignee && (
              <span className="ui-board-card-who">
                {c.assignee.kind === 'agent' ? (
                  <AgentDot label={assigneeLabel(t, c.assignee)} />
                ) : (
                  <Avatar size="sm" name={assigneeLabel(t, c.assignee)} />
                )}
              </span>
            )}
          </span>
        )}
      </button>
      {canWrite && <MoveMenu card={c} all={all} shown={shownInColumn} onMove={onMove} />}
    </li>
  )
}

export function dueWords(t: T, state: DueState, due: string, today: string, f: Format): string {
  if (state === 'overdue') return t('boards.overdueOn', { date: f.calendarDate(due) })
  if (state === 'soon') return t(due === today ? 'boards.dueToday' : 'boards.dueTomorrow')
  return t('boards.dueOn', { date: f.calendarDate(due) })
}

function dueName(t: T, state: DueState, due: string, today: string, f: Format): string {
  if (state === 'overdue') return t('boards.nameOverdue', { date: f.calendarDate(due) })
  if (state === 'soon') return t(due === today ? 'boards.nameDueToday' : 'boards.nameDueTomorrow')
  return t('boards.nameDue', { date: f.calendarDate(due) })
}

export function DueBadge({ state, text }: { state: 'overdue' | 'soon' | 'later'; text: string }) {
  if (state === 'later')
    return (
      <span className="ui-board-due">
        <Icon name="clock" size={14} />
        {text}
      </span>
    )
  return (
    <Badge tone={state === 'overdue' ? 'danger' : 'warning'} icon={state === 'overdue' ? 'alert' : 'clock'}>
      {text}
    </Badge>
  )
}

function MoveMenu({
  card: c,
  all,
  shown,
  onMove,
}: {
  card: Card
  all: Card[]
  shown: Card[]
  onMove: (column: Card['column'], before?: Card) => void
}) {
  const t = useT()
  const here = t(COLUMNS.find((col) => col.id === c.column)?.label ?? 'boards.columns.backlog')
  const first = shown[0]
  const toTop = first && dropPosition(all, c, c.column, first) !== null
  const toBottom = dropPosition(all, c, c.column) !== null
  return (
    <span className="ui-board-card-menu">
      <Menu>
        <MenuButton size="sm" icon="move" label={t('boards.moveCard', { title: c.title })} />
        <MenuList>
          {toTop && (
            <MenuItem icon="up" onSelect={() => onMove(c.column, first)}>
              {t('boards.moveTop', { column: here })}
            </MenuItem>
          )}
          {toBottom && (
            <MenuItem icon="down" onSelect={() => onMove(c.column)}>
              {t('boards.moveBottom', { column: here })}
            </MenuItem>
          )}
          {(toTop || toBottom) && <MenuSeparator />}
          {COLUMNS.filter((col) => col.id !== c.column).map((col) => (
            <MenuItem key={col.id} onSelect={() => onMove(col.id)}>
              {t('boards.moveTo', { column: t(col.label) })}
            </MenuItem>
          ))}
        </MenuList>
      </Menu>
    </span>
  )
}
