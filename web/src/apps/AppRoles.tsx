// The roles of one app: who may build it, publish it, add records to it and read or change
// its records, among the members of its space. Roles only narrow what a member's role in the
// space allows. An app with no roles set is unrestricted, as it was before roles existed.
// Shown to whoever may manage the app's roles; the server checks it again on every change.

import { useState } from 'react'
import { type AppGrant, type AppRole, api, errorText, type SpaceMember } from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'

const ROLES: AppRole[] = ['builder', 'publisher', 'user', 'data_manager', 'reader']
const DEFAULT = 'default'

// Who the roles are for: a member of the space, or everyone without roles of their own.
type Who = { key: string; name: string }

export function AppRoles({ base, spaceBase, onClose }: { base: string; spaceBase: string; onClose: () => void }) {
  const t = useT()
  const loaded = useLoad(
    () =>
      Promise.all([
        api<{ grants: AppGrant[] }>(`${base}/roles`),
        api<{ members: SpaceMember[] }>(`${spaceBase}/members`),
      ]),
    [base, spaceBase],
  )
  if (loaded.error) {
    return (
      <p role="alert" className="tt-error">
        {loaded.error}
      </p>
    )
  }
  if (!loaded.data) return <p className="tt-muted">{t('app.loading')}</p>
  const [roles, members] = loaded.data
  return <RolesForm base={base} grants={roles.grants} members={members.members} onClose={onClose} />
}

function RolesForm({
  base,
  grants,
  members,
  onClose,
}: {
  base: string
  grants: AppGrant[]
  members: SpaceMember[]
  onClose: () => void
}) {
  const t = useT()
  const who: Who[] = [
    { key: DEFAULT, name: t('apps.roles.everyoneElse') },
    ...members.map((m) => ({ key: String(m.id), name: m.name })),
  ]
  const [chosen, setChosen] = useState<Record<string, AppRole[]>>(() =>
    Object.fromEntries(grants.map((g) => [g.user_id === null ? DEFAULT : String(g.user_id), g.roles])),
  )
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [saved, setSaved] = useState(false)

  const toggle = (key: string, role: AppRole) => {
    setSaved(false)
    setChosen((c) => {
      const has = c[key] ?? []
      return { ...c, [key]: has.includes(role) ? has.filter((r) => r !== role) : [...has, role] }
    })
  }
  const save = async () => {
    setBusy(true)
    const body = {
      // In the order shown: the default first, then the members.
      grants: who
        .filter((w) => (chosen[w.key] ?? []).length > 0)
        .map((w) => ({ user_id: w.key === DEFAULT ? null : Number(w.key), roles: chosen[w.key] })),
    }
    try {
      await api(`${base}/roles`, { method: 'PUT', body })
      setError(null)
      setSaved(true)
    } catch (e) {
      setError(errorText(e))
    }
    setBusy(false)
  }

  return (
    <div className="tt-app-roles">
      <p className="tt-muted tt-small">{t('apps.roles.intro')}</p>
      <ul className="tt-muted tt-small">
        {ROLES.map((r) => (
          <li key={r}>{t('apps.roles.line', { role: t(`apps.roles.${r}`), help: t(`apps.roles.${r}Help`) })}</li>
        ))}
      </ul>
      <table>
        <thead>
          <tr>
            <th scope="col">{t('apps.roles.member')}</th>
            {ROLES.map((r) => (
              <th key={r} scope="col">
                {t(`apps.roles.${r}`)}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {who.map((w) => (
            <tr key={w.key}>
              <th scope="row">{w.name}</th>
              {ROLES.map((r) => (
                <td key={r}>
                  <input
                    type="checkbox"
                    checked={(chosen[w.key] ?? []).includes(r)}
                    onChange={() => toggle(w.key, r)}
                    aria-label={t('apps.roles.checkbox', { role: t(`apps.roles.${r}`), name: w.name })}
                  />
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      {saved && (
        <p role="status" className="tt-muted tt-small">
          {t('apps.roles.saved')}
        </p>
      )}
      <div className="tt-app-actions">
        <button type="button" className="tt-button tt-primary" disabled={busy} onClick={save}>
          {t('apps.save')}
        </button>
        <button type="button" className="tt-button" onClick={onClose}>
          {t('apps.roles.close')}
        </button>
      </div>
    </div>
  )
}
