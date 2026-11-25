import type { DragEvent } from 'react'
import type { Card } from '../api'
import { type Format, useT } from '../i18n'
import { AgentDot } from '../ui/AgentDot'
import { Avatar } from '../ui/Avatar'
import { Badge } from '../ui/Badge'
import { Icon } from '../ui/Icon'
import { Label } from '../ui/Label'
import { Menu, MenuButton, MenuItem, MenuList, MenuSeparator } from '../ui/Menu'
import { COLUMNS } from './columns'
import { dueState } from './due'
import { dropPosition } from './move'
import { assigneeLabel } from './text'

// A card on the board: a button that opens it (its title, due date, labels, whether it is
// blocked, and who it is for), draggable by a writer with a mouse, a place to drop another card
// before it, and a writer's Move menu, which moves it by keyboard or on touch: to the top or the
// bottom of its column, or to another column (last there, as a drop on the column puts it).
export function BoardCard({
  card: c,
  all,
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
            {due && c.due && <DueBadge state={due} text={dueText(t, due, f.calendarDate(c.due))} />}
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
      {canWrite && <MoveMenu card={c} all={all} onMove={onMove} />}
    </li>
  )
}

const dueText = (t: ReturnType<typeof useT>, state: 'overdue' | 'soon' | 'later', date: string) =>
  t(state === 'overdue' ? 'boards.overdueOn' : 'boards.dueOn', { date })

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
// it isn't there already), and the other columns. Each goes the way a drop there would.
function MoveMenu({
  card: c,
  all,
  onMove,
}: {
  card: Card
  all: Card[]
  onMove: (column: Card['column'], before?: Card) => void
}) {
  const t = useT()
  const here = t(COLUMNS.find((col) => col.id === c.column)?.label ?? 'boards.columns.backlog')
  const first = all.find((x) => x.column === c.column)
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
