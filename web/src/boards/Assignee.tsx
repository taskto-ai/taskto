import type { Card } from '../api'
import { useT } from '../i18n'
import { AgentDot } from '../ui/AgentDot'
import { Avatar } from '../ui/Avatar'
import { Menu, MenuButton, MenuItem, MenuList } from '../ui/Menu'
import type { Person } from './people'
import { assigneeLabel } from './text'

// Who the card is for: nobody, the agent, or someone who can read the space. A writer picks from
// a menu. Someone assigned who has since lost access stays listed until the assignee is changed.
export function Assignee({
  card,
  canWrite,
  people,
  busy,
  onSet,
}: {
  card: Card
  canWrite: boolean
  people: Person[] | undefined
  busy: boolean
  onSet: (assignee: Card['assignee']) => void
}) {
  const a = card.assignee
  const t = useT()
  const shown = a ? <Who assignee={a} /> : <span className="ui-board-muted">{t('boards.nobody')}</span>
  if (!canWrite) return shown

  const value = !a ? '' : a.kind === 'agent' ? 'agent' : `user:${a.id}`
  const listed = people ?? []
  const others =
    a?.kind === 'user' && !listed.some((p) => p.id === a.id) ? [{ id: a.id, name: assigneeLabel(t, a) }] : []
  const choose = (v: string, assignee: Card['assignee']) => {
    if (v !== value) onSet(assignee)
  }
  const mark = (v: string) => (v === value ? ('check' as const) : undefined)
  return (
    <Menu align="start">
      <MenuButton size="sm" label={t('boards.assignee')} disabled={busy} className="ui-board-value-button">
        {shown}
      </MenuButton>
      <MenuList>
        <MenuItem icon={mark('')} onSelect={() => choose('', null)}>
          {t('boards.nobody')}
        </MenuItem>
        <MenuItem icon={mark('agent')} onSelect={() => choose('agent', { kind: 'agent' })}>
          <span className="ui-board-value">
            <AgentDot />
            {t('boards.agent')}
          </span>
        </MenuItem>
        {[...others, ...listed].map((p) => (
          <MenuItem
            key={p.id}
            icon={mark(`user:${p.id}`)}
            onSelect={() => choose(`user:${p.id}`, { kind: 'user', id: p.id })}
          >
            <span className="ui-board-value">
              <Avatar size="sm" name={p.name} decorative />
              {p.name}
            </span>
          </MenuItem>
        ))}
      </MenuList>
    </Menu>
  )
}

// The person the card is for, or the agent, with their mark.
function Who({ assignee }: { assignee: NonNullable<Card['assignee']> }) {
  const t = useT()
  const name = assigneeLabel(t, assignee)
  return (
    <span className="ui-board-value">
      {assignee.kind === 'agent' ? <AgentDot /> : <Avatar size="sm" name={name} decorative />}
      {name}
    </span>
  )
}
