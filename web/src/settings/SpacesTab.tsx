// Shared spaces, for admins: create, rename, archive, and who is in each. Admins manage a
// space's members without reading it; one who wants to read it adds themselves, which every
// member sees in the list. Private spaces are nobody else's business and aren't listed.

import { type FormEvent, useState } from 'react'
import { api, type Member, type Space, type SpaceMember, ws } from '../api'
import { type T, useT } from '../i18n'
import { useLoad } from '../load'
import { Alert, ConfirmButton, useAction } from './parts'

type SpaceRole = SpaceMember['role']

// "You: editor", a whole phrase per role.
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
    <div className="tt-settings">
      <section className="tt-panel" aria-labelledby="tt-spaces">
        <h2 id="tt-spaces">{t('settings.spaces.title')}</h2>
        <CreateSpace
          base={base}
          onCreated={(id) => {
            setOpen(id)
            spaces.reload()
          }}
        />
        <Alert error={spaces.error} />
        {!spaces.data && !spaces.error && <p className="tt-muted">{t('app.loading')}</p>}
        {spaces.data?.length === 0 && <p className="tt-muted">{t('settings.spaces.none')}</p>}
        {spaces.data && spaces.data.length > 0 && (
          <ul className="tt-list" aria-label={t('settings.spaces.title')}>
            {spaces.data.map((s) => (
              <li key={s.id}>
                <button type="button" aria-current={s.id === open || undefined} onClick={() => setOpen(s.id)}>
                  <span className="tt-list-title">{s.name}</span>
                  <span className="tt-muted tt-small">
                    {s.archived_at ? `${t('settings.spaces.archived')} · ` : ''}
                    {s.role ? yourRole(t, s.role) : t('settings.spaces.notMember')}
                  </span>
                </button>
              </li>
            ))}
          </ul>
        )}
      </section>
      {space && (
        <SpaceSettings key={space.id} base={base} workspace={workspace} space={space} onChange={spaces.reload} />
      )}
    </div>
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
    <>
      <form className="tt-form tt-form-row" onSubmit={submit}>
        <label>
          {t('settings.spaces.new')}
          <input value={name} onChange={(e) => setName(e.target.value)} required maxLength={100} />
        </label>
        <button type="submit" className="tt-button tt-primary" disabled={action.busy}>
          {t('settings.spaces.create')}
        </button>
      </form>
      <Alert error={action.error} />
    </>
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
  const patch = (body: object) => action.run(() => api(url, { method: 'PATCH', body })).then(onChange)
  const rename = (e: FormEvent) => {
    e.preventDefault()
    patch({ name })
  }

  return (
    <section className="tt-panel" aria-label={space.name}>
      <h2>{space.name}</h2>
      {space.archived_at && <p className="tt-muted">{t('settings.spaces.archivedHelp')}</p>}
      <form className="tt-form tt-form-row" onSubmit={rename}>
        <label>
          {t('settings.spaces.name')}
          <input value={name} onChange={(e) => setName(e.target.value)} required maxLength={100} />
        </label>
        <button type="submit" className="tt-button" disabled={action.busy || name.trim() === space.name}>
          {t('settings.spaces.rename')}
        </button>
      </form>
      <div className="tt-row-actions">
        {space.archived_at ? (
          <button type="button" className="tt-button" disabled={action.busy} onClick={() => patch({ archived: false })}>
            {t('settings.spaces.unarchive')}
          </button>
        ) : (
          <ConfirmButton
            label={t('settings.spaces.archive')}
            question={t('settings.spaces.archiveQuestion', { name: space.name })}
            confirm={t('settings.spaces.archiveConfirm')}
            disabled={action.busy}
            onConfirm={() => patch({ archived: true })}
          />
        )}
      </div>
      <label>
        <input
          type="checkbox"
          checked={space.offline_downloads !== false}
          disabled={action.busy}
          onChange={(e) => patch({ offline_downloads: e.target.checked })}
        />
        {t('settings.spaces.offlineDownloads')}
      </label>
      <p className="tt-muted tt-small">{t('settings.spaces.offlineHelp')}</p>
      <Alert error={action.error} />
      <SpaceMembers workspace={workspace} url={`${url}/members`} onChange={onChange} />
    </section>
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
  // The caller's own role shows in the space list: it changes with these.
  const done = () => {
    members.reload()
    onChange()
  }

  const setRole = (m: SpaceMember, role: SpaceRole) =>
    action.run(() => api(`${url}/${m.id}`, { method: 'PATCH', body: { role } })).then(done)
  const remove = (m: SpaceMember) => action.run(() => api(`${url}/${m.id}`, { method: 'DELETE' })).then(done)
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
    <>
      <h3>{t('settings.spaces.members')}</h3>
      <Alert error={members.error ?? everyone.error ?? action.error} />
      {members.data?.length === 0 && <p className="tt-muted">{t('settings.spaces.nobody')}</p>}
      {members.data && members.data.length > 0 && (
        <ul className="tt-rows" aria-label={t('settings.spaces.spaceMembers')}>
          {members.data.map((m) => (
            <li key={m.id} className="tt-row">
              <div className="tt-row-main">
                <strong>{m.name}</strong>
                <span className="tt-muted tt-small">{m.email}</span>
              </div>
              <div className="tt-row-actions">
                <label>
                  <span className="tt-sr-only">{t('settings.spaces.roleOf', { name: m.name })}</span>
                  <select
                    value={m.role}
                    disabled={action.busy}
                    onChange={(e) => setRole(m, e.target.value as SpaceRole)}
                  >
                    <option value="editor">{t('settings.spaces.roles.editor')}</option>
                    <option value="viewer">{t('settings.spaces.roles.viewer')}</option>
                  </select>
                </label>
                <ConfirmButton
                  label={t('settings.members.remove')}
                  name={m.name}
                  question={t('settings.spaces.removeQuestion', { name: m.name })}
                  confirm={t('settings.members.removeConfirm')}
                  disabled={action.busy}
                  onConfirm={() => remove(m)}
                />
              </div>
            </li>
          ))}
        </ul>
      )}
      {members.data && outside.length > 0 && (
        <form className="tt-form tt-form-row" onSubmit={add}>
          <label>
            {t('settings.spaces.addMember')}
            <select value={adding.user} onChange={(e) => setAdding((a) => ({ ...a, user: e.target.value }))} required>
              <option value="">{t('settings.spaces.choose')}</option>
              {outside.map((p) => (
                <option key={p.id} value={p.id}>
                  {t('settings.spaces.person', { name: p.name, email: p.email })}
                </option>
              ))}
            </select>
          </label>
          <label>
            {t('settings.spaces.as')}
            <select
              value={adding.role}
              onChange={(e) => setAdding((a) => ({ ...a, role: e.target.value as SpaceRole }))}
            >
              <option value="editor">{t('settings.spaces.roles.editor')}</option>
              <option value="viewer">{t('settings.spaces.roles.viewer')}</option>
            </select>
          </label>
          <button type="submit" className="tt-button" disabled={action.busy}>
            {t('settings.spaces.add')}
          </button>
        </form>
      )}
      <p className="tt-muted tt-small">{t('settings.spaces.rolesHelp')}</p>
    </>
  )
}
