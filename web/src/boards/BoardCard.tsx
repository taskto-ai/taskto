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

// A card on the board: a button that opens it (its title, due date, labels, whether it is
// blocked, and who it is for), draggable by a writer with a mouse, a place to drop another card
// before it, and a writer's Move menu, which moves it by keyboard or on touch: to the top or the
// bottom of its column, or to another column (last there, as a drop on the column puts it). A
// screen reader hears a short name: its title, whether it is blocked, and when it is due; the
// reason and the labels are the open card's to say.
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
  // The cards of its column the filters show, in order.
  shownInColumn: Card[]
  canWrite: boolean
  // Whether it is the open card.
  current: boolean
  dragged: boolean
  today: string
  // The board's formats, made once for all its cards.
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
  const name = details.length > 0 ? t('boards.cardName', { title: c.title, details: f.list(details) }) : undefined
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

// A due date in words: "Overdue: Mar 1", "Due today", "Due tomorrow", "Due Mar 9".
export function dueWords(t: T, state: DueState, due: string, today: string, f: Format): string {
  if (state === 'overdue') return t('boards.overdueOn', { date: f.calendarDate(due) })
  if (state === 'soon') return t(due === today ? 'boards.dueToday' : 'boards.dueTomorrow')
  return t('boards.dueOn', { date: f.calendarDate(due) })
}

// The same, as part of the card's name for a screen reader: "overdue since Mar 1", "due today".
function dueName(t: T, state: DueState, due: string, today: string, f: Format): string {
  if (state === 'overdue') return t('boards.nameOverdue', { date: f.calendarDate(due) })
  if (state === 'soon') return t(due === today ? 'boards.nameDueToday' : 'boards.nameDueTomorrow')
  return t('boards.nameDue', { date: f.calendarDate(due) })
}

// A due date as the board shows it: overdue in the danger tone, due soon in the warning tone,
// later ones quiet. The words say which, the colour only helps.
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

// The places a card can move to without a mouse: the top and the bottom of its column (each when
// it isn't there already), and the other columns. Each goes the way a drop there would: the top
// is a drop on the first card shown (with a filter on, cards it hides may stay above it), the
// bottom or another column a drop on the column.
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
