import type { Card } from '../api'
import { useT } from '../i18n'
import type { Person } from './people'
import { assigneeLabel } from './text'

// Who the card is for: nobody, the agent, or someone who can read the space. Someone assigned
// who has since lost access stays listed until the assignee is changed.
export function Assignee({
  card,
  people,
  busy,
  onSet,
}: {
  card: Card
  people: Person[] | undefined
  busy: boolean
  onSet: (assignee: Card['assignee']) => void
}) {
  const a = card.assignee
  const t = useT()
  const value = !a ? '' : a.kind === 'deleted' ? 'deleted' : a.kind === 'agent' ? 'agent' : `user:${a.id}`
  const listed = people ?? []
  const others =
    a?.kind === 'user' && !listed.some((p) => p.id === a.id) ? [{ id: a.id, name: assigneeLabel(t, a) }] : []
  return (
    <label className="tt-inline-field">
      {t('boards.assignee')}
      <select
        value={value}
        disabled={busy}
        onChange={(e) => {
          const v = e.target.value
          onSet(v === '' ? null : v === 'agent' ? { kind: 'agent' } : { kind: 'user', id: Number(v.slice(5)) })
        }}
      >
        {/* The account that was assigned is gone: shown as it is, not pickable; choosing anyone else replaces it. */}
        {a?.kind === 'deleted' && (
          <option value="deleted" disabled>
            {t('common.deletedUser')}
          </option>
        )}
        <option value="">{t('boards.nobody')}</option>
        <option value="agent">{t('boards.agent')}</option>
        {[...others, ...listed].map((p) => (
          <option key={p.id} value={`user:${p.id}`}>
            {p.name}
          </option>
        ))}
      </select>
    </label>
  )
}
