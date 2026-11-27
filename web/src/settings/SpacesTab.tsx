import { type FormEvent, useState } from 'react'
import { api, type Member, type Space, type SpaceMember, ws } from '../api'
import { type T, useT } from '../i18n'
import { useLoad } from '../load'
import { Avatar } from '../ui/Avatar'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { useConfirm } from '../ui/ConfirmDialog'
import { Dialog } from '../ui/Dialog'
import { EmptyState } from '../ui/EmptyState'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { Select } from '../ui/Select'
import { Switch } from '../ui/Switch'
import { Alert, Loading, Section, SettingsTab, useAction } from './parts'

type SpaceRole = SpaceMember['role']

function yourRole(t: T, role: string): string {
  if (role === 'owner') return t('settings.spaces.you.owner')
  if (role === 'viewer') return t('settings.spaces.you.viewer')
  return t('settings.spaces.you.editor')
}

export function SpacesTab({ workspace }: { workspace: number }) {
  const base = `${ws(workspace)}/spaces`
  const spaces = useLoad(
    () => api<{ spaces: Space[] }>(`${base}?all=true`).then((r) => r.spaces.filter((s) => s.kind === 'shared')),
    [base],
  )
  const [open, setOpen] = useState<number | null>(null)
  const space = spaces.data?.find((s) => s.id === open)
  const t = useT()

  return (
    <SettingsTab title={t('settings.spaces.title')} help={t('settings.help.spaces')}>
      <Panel padded>
        <CreateSpace
          base={base}
          onCreated={(id) => {
            setOpen(id)
            spaces.reload()
          }}
        />
      </Panel>
      <Alert error={spaces.error} />
      {!spaces.data && !spaces.error && <Loading />}
      {spaces.data?.length === 0 && (
        <EmptyState title={t('settings.spaces.none')}>{t('settings.spaces.noneHelp')}</EmptyState>
      )}
      {spaces.data && spaces.data.length > 0 && (
        <Panel>
          <Rows label={t('settings.spaces.title')}>
            {spaces.data.map((s) => (
              <Row
                key={s.id}
                leading={<Avatar name={s.name} size="sm" shape="square" decorative />}
                title={s.name}
                detail={s.role ? yourRole(t, s.role) : t('settings.spaces.notMember')}
                status={s.archived_at && <Badge>{t('settings.spaces.archived')}</Badge>}
                actions={
                  <Button
                    size="sm"
                    aria-label={t('common.labelFor', { label: t('settings.spaces.manage'), name: s.name })}
                    onClick={() => setOpen(s.id)}
                  >
                    {t('settings.spaces.manage')}
                  </Button>
                }
              />
            ))}
          </Rows>
        </Panel>
      )}
      {space && (
        <Dialog open onClose={() => setOpen(null)} title={space.name} size="lg">
          <SpaceSettings key={space.id} base={base} workspace={workspace} space={space} onChange={spaces.reload} />
        </Dialog>
      )}
    </SettingsTab>
  )
}

function CreateSpace({ base, onCreated }: { base: string; onCreated: (id: number) => void }) {
  const t = useT()
  const [name, setName] = useState('')
  const action = useAction()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    let made: Space | undefined
    if (await action.run(async () => (made = await api<Space>(base, { method: 'POST', body: { name } })))) {
      setName('')
      if (made) onCreated(made.id)
    }
  }
  return (
    <form className="settings-form" onSubmit={submit}>
      <div className="settings-form-row">
        <Field label={t('settings.spaces.new')} required>
          <Input value={name} onChange={(e) => setName(e.target.value)} maxLength={100} />
        </Field>
        <Button type="submit" variant="primary" icon="plus" loading={action.busy}>
          {t('settings.spaces.create')}
        </Button>
      </div>
      <Alert error={action.error} />
    </form>
  )
}

function SpaceSettings({
  base,
  workspace,
  space,
  onChange,
}: {
  base: string
  workspace: number
  space: Space
  onChange: () => void
}) {
  const url = `${base}/${space.id}`
  const t = useT()
  const [name, setName] = useState(space.name)
  const action = useAction()
  const [confirmDialog, confirm] = useConfirm()
  const patch = (body: object) => action.run(() => api(url, { method: 'PATCH', body })).then(onChange)
  const rename = (e: FormEvent) => {
    e.preventDefault()
    patch({ name })
  }
  const archive = async () => {
    const yes = await confirm({
      title: t('settings.spaces.archiveQuestion', { name: space.name }),
      confirmLabel: t('settings.spaces.archiveConfirm'),
      tone: 'danger',
    })
    if (yes) patch({ archived: true })
  }

  return (
    <div className="settings-panel-body">
      {space.archived_at && <Notice tone="neutral">{t('settings.spaces.archivedHelp')}</Notice>}
      <form className="settings-form" onSubmit={rename}>
        <div className="settings-form-row">
          <Field label={t('settings.spaces.name')} required>
            <Input value={name} onChange={(e) => setName(e.target.value)} maxLength={100} />
          </Field>
          <Button type="submit" disabled={action.busy || name.trim() === space.name}>
            {t('settings.spaces.rename')}
          </Button>
        </div>
      </form>
      <div className="settings-switch-row">
        <Switch
          label={t('settings.spaces.offlineDownloads')}
          checked={space.offline_downloads !== false}
          disabled={action.busy}
          onChange={(e) => patch({ offline_downloads: e.target.checked })}
        />
        <p className="settings-muted">{t('settings.spaces.offlineHelp')}</p>
      </div>
      <div className="settings-actions">
        {space.archived_at ? (
          <Button icon="archive" disabled={action.busy} onClick={() => patch({ archived: false })}>
            {t('settings.spaces.unarchive')}
          </Button>
        ) : (
          <Button variant="danger" icon="archive" disabled={action.busy} onClick={archive}>
            {t('settings.spaces.archive')}
          </Button>
        )}
      </div>
      <Alert error={action.error} />
      <SpaceMembers workspace={workspace} url={`${url}/members`} onChange={onChange} />
      {confirmDialog}
    </div>
  )
}

function SpaceMembers({ workspace, url, onChange }: { workspace: number; url: string; onChange: () => void }) {
  const members = useLoad(() => api<{ members: SpaceMember[] }>(url).then((r) => r.members), [url])
  const everyone = useLoad(
    () => api<{ members: Member[] }>(`${ws(workspace)}/members`).then((r) => r.members),
    [workspace],
  )
  const [adding, setAdding] = useState<{ user: string; role: SpaceRole }>({ user: '', role: 'editor' })
  const t = useT()
  const action = useAction()
  const [confirmDialog, confirm] = useConfirm()
  const done = () => {
    members.reload()
    onChange()
  }

  const setRole = (m: SpaceMember, role: SpaceRole) =>
    action.run(() => api(`${url}/${m.id}`, { method: 'PATCH', body: { role } })).then(done)
  const remove = async (m: SpaceMember) => {
    const yes = await confirm({
      title: t('settings.spaces.removeQuestion', { name: m.name }),
      confirmLabel: t('settings.members.removeConfirm'),
      tone: 'danger',
    })
    if (yes) action.run(() => api(`${url}/${m.id}`, { method: 'DELETE' })).then(done)
  }
  const add = async (e: FormEvent) => {
    e.preventDefault()
    const ok = await action.run(() =>
      api(url, { method: 'POST', body: { user_id: Number(adding.user), role: adding.role } }),
    )
    if (ok) setAdding((a) => ({ ...a, user: '' }))
    done()
  }
  const outside = everyone.data?.filter((p) => !members.data?.some((m) => m.id === p.id)) ?? []

  return (
    <Section title={t('settings.spaces.members')} help={t('settings.spaces.rolesHelp')} level={3}>
      <Alert error={members.error ?? everyone.error ?? action.error} />
      {!members.data && !members.error && <Loading lines={2} />}
      {members.data?.length === 0 && <p className="settings-muted">{t('settings.spaces.nobody')}</p>}
      {members.data && members.data.length > 0 && (
        <Panel>
          <Rows label={t('settings.spaces.spaceMembers')}>
            {members.data.map((m) => (
              <Row
                key={m.id}
                leading={<Avatar name={m.name} size="sm" decorative />}
                title={m.name}
                detail={m.email}
                actions={
                  <>
                    <Select
                      size="sm"
                      className="settings-role"
                      aria-label={t('settings.spaces.roleOf', { name: m.name })}
                      value={m.role}
                      disabled={action.busy}
                      onChange={(e) => setRole(m, e.target.value as SpaceRole)}
                    >
                      <option value="editor">{t('settings.spaces.roles.editor')}</option>
                      <option value="viewer">{t('settings.spaces.roles.viewer')}</option>
                    </Select>
                    <Button
                      variant="ghost"
                      size="sm"
                      aria-label={t('common.labelFor', { label: t('settings.members.remove'), name: m.name })}
                      disabled={action.busy}
                      onClick={() => remove(m)}
                    >
                      {t('settings.members.remove')}
                    </Button>
                  </>
                }
              />
            ))}
          </Rows>
        </Panel>
      )}
      {members.data && outside.length > 0 && (
        <form className="settings-form" onSubmit={add}>
          <div className="settings-form-row">
            <Field label={t('settings.spaces.addMember')} required>
              <Select value={adding.user} onChange={(e) => setAdding((a) => ({ ...a, user: e.target.value }))}>
                <option value="">{t('settings.spaces.choose')}</option>
                {outside.map((p) => (
                  <option key={p.id} value={p.id}>
                    {t('settings.spaces.person', { name: p.name, email: p.email })}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label={t('settings.spaces.as')} className="settings-field-narrow">
              <Select
                value={adding.role}
                onChange={(e) => setAdding((a) => ({ ...a, role: e.target.value as SpaceRole }))}
              >
                <option value="editor">{t('settings.spaces.roles.editor')}</option>
                <option value="viewer">{t('settings.spaces.roles.viewer')}</option>
              </Select>
            </Field>
            <Button type="submit" icon="plus" loading={action.busy}>
              {t('settings.spaces.add')}
            </Button>
          </div>
        </form>
      )}
      {confirmDialog}
    </Section>
  )
}
