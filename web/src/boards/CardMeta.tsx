import type { ReactNode } from 'react'
import type { Card } from '../api'
import { useT } from '../i18n'
import { Menu, MenuButton, MenuItem, MenuList } from '../ui/Menu'
import { COLUMN_LABELS, COLUMNS } from './columns'
import { StatusDot } from './StatusDot'

// A row of the open card's properties: its name, and its value with the control that changes it.
// The row is a focus group: when a control in it goes, the focus stays in the row (focus.ts).
export function Prop({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="ui-board-prop" data-focus-group>
      <dt>{label}</dt>
      <dd>{children}</dd>
    </div>
  )
}

// The card's status, its column: a writer moves it with the menu, as a drag does on the board.
// An archived card is restored from the card's menu, not moved.
export function Status({
  card,
  canWrite,
  busy,
  onMove,
}: {
  card: Card
  canWrite: boolean
  busy: boolean
  onMove: (column: Card['column']) => void
}) {
  const t = useT()
  const name = t(COLUMN_LABELS[card.column])
  if (!canWrite || card.column === 'archived')
    return (
      <span className="ui-board-value">
        <StatusDot column={card.column} />
        {name}
      </span>
    )
  return (
    <Menu align="start">
      <MenuButton size="sm" label={t('boards.status')} disabled={busy} className="ui-board-value-button">
        <StatusDot column={card.column} />
        {name}
      </MenuButton>
      <MenuList>
        {COLUMNS.map((c) => (
          <MenuItem
            key={c.id}
            icon={c.id === card.column ? 'check' : undefined}
            onSelect={() => c.id !== card.column && onMove(c.id)}
          >
            <span className="ui-board-value">
              <StatusDot column={c.id} />
              {t(c.label)}
            </span>
          </MenuItem>
        ))}
      </MenuList>
    </Menu>
  )
}
