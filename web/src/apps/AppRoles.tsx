// The roles of one app: who may build it, publish it, add records to it and read or change
// its records, among the members of its space. Roles only narrow what a member's role in the
// space allows. An app with no roles set is unrestricted, as it was before roles existed.
// Shown to whoever may manage the app's roles, in a dialog the Apps page opens; the server checks
// it again on every change.

import { useState } from 'react'
import { type AppGrant, type AppRole, api, errorText, type SpaceMember } from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { Button } from '../ui/Button'
import { Checkbox } from '../ui/Checkbox'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { type Column, Table } from '../ui/Table'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { LoadingRows } from './AppsLoading'
import './Apps.css'

const ROLES: AppRole[] = ['builder', 'publisher', 'user', 'data_manager', 'reader']
const DEFAULT = 'default'

// Who the roles are for: a member of the space, or everyone without roles of their own.
type Who = { key: string; name: string }

export function AppRoles({ base, spaceBase }: { base: string; spaceBase: string }) {
  const loaded = useLoad(
    () =>
      Promise.all([
        api<{ grants: AppGrant[] }>(`${base}/roles`),
        api<{ members: SpaceMember[] }>(`${spaceBase}/members`),
      ]),
    [base, spaceBase],
  )
  if (loaded.error) return <Notice tone="danger">{loaded.error}</Notice>
  if (!loaded.data) return <LoadingRows count={3} />
  const [roles, members] = loaded.data
  return <RolesForm base={base} grants={roles.grants} members={members.members} />
}

function RolesForm({ base, grants, members }: { base: string; grants: AppGrant[]; members: SpaceMember[] }) {
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

  const columns: Column<Who>[] = [
    { key: 'who', header: t('apps.roles.member'), rowHeader: true, cell: (w) => w.name },
    ...ROLES.map((r) => ({
      key: r,
      header: t(`apps.roles.${r}`),
      cell: (w: Who) => (
        <Checkbox
          checked={(chosen[w.key] ?? []).includes(r)}
          onChange={() => toggle(w.key, r)}
          label={
            <VisuallyHidden>{t('apps.roles.checkbox', { role: t(`apps.roles.${r}`), name: w.name })}</VisuallyHidden>
          }
        />
      ),
    })),
  ]

  return (
    <div className="apps-stack">
      <p className="apps-note">{t('apps.roles.intro')}</p>
      <ul className="apps-list apps-note">
        {ROLES.map((r) => (
          <li key={r}>{t('apps.roles.line', { role: t(`apps.roles.${r}`), help: t(`apps.roles.${r}Help`) })}</li>
        ))}
      </ul>
      <Panel>
        <Table caption={t('apps.roles.caption')} columns={columns} rows={who} rowKey={(w) => w.key} />
      </Panel>
      {error && <Notice tone="danger">{error}</Notice>}
      <div className="apps-actions">
        <Button variant="primary" loading={busy} onClick={save}>
          {t('apps.save')}
        </Button>
        {saved && (
          <p role="status" className="apps-note">
            {t('apps.roles.saved')}
          </p>
        )}
      </div>
    </div>
  )
}
