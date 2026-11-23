// Starting an app from a template, and copying one. A template (or a copy) is only a definition: the
// server makes a new, independent app from it in the space the person picks (their private space
// unless they choose a shared one), as a draft, with no records, photos, history or roles of anyone
// else's. The person then tries the draft and publishes it in the ordinary way, with the panel that
// says who will be able to use it. A template whose effects make cards needs a board the person may
// write, chosen here; the server checks it again, and so it does at publishing.

import { type FormEvent, useState } from 'react'
import { type App, type AppRole, type AppTemplate, api, type Board, errorText, type Space, ws } from '../api'
import { has, type Key, useT } from '../i18n'
import { useLoad } from '../load'
import { Link, navigate, routePath } from '../router'
import { spaceLabel } from '../spaces'
import { appError, PublishPanel } from './AppPublish'
import { AppStage } from './AppStage'
import { installedPath } from './route'

// The template's own name and description in the user's language, when we have them; the server's
// English text otherwise (a template added later still shows).
function templateText(tpl: AppTemplate, part: 'name' | 'description', t: (k: Key) => string): string {
  const key = `apps.templates.items.${tpl.id.replaceAll('-', '_')}.${part}`
  return has(key) ? t(key as Key) : tpl[part]
}

// A board somebody may write, with where it is: the choices of an effect's target.
export type BoardChoice = { space: Space; board: Board }

// The boards of every space the user may write (the server answers only for those it lets them see,
// and checks the one chosen again).
export function useWritableBoards(workspace: number, spaces: Space[] | undefined) {
  const writable = (spaces ?? []).filter((s) => s.access === 'write' && !s.archived_at)
  const key = writable.map((s) => s.id).join(',')
  return useLoad<BoardChoice[]>(
    async () => {
      const all = await Promise.all(
        writable.map(async (space) => {
          const r = await api<{ boards: Board[] }>(`${ws(workspace)}/spaces/${space.id}/boards`).catch(() => ({
            boards: [] as Board[],
          }))
          return r.boards.filter((b) => !b.archived_at).map((board) => ({ space, board }))
        }),
      )
      return all.flat()
    },
    [workspace, key], // key stands for the writable spaces
  )
}

function BoardSelect({
  id,
  choices,
  value,
  onChange,
}: {
  id: string
  choices: BoardChoice[]
  value: string
  onChange: (v: string) => void
}) {
  const t = useT()
  return (
    <select id={id} value={value} onChange={(e) => onChange(e.target.value)} required>
      <option value="">{t('apps.templates.chooseBoard')}</option>
      {choices.map((c) => (
        <option key={`${c.space.id}:${c.board.id}`} value={`${c.space.id}:${c.board.id}`}>
          {t('apps.templates.boardIn', { board: c.board.name, space: spaceLabel(c.space) })}
        </option>
      ))}
    </select>
  )
}

function parseChoice(v: string): { space_id: number; board_id: number } | null {
  const [s, b] = v.split(':').map(Number)
  return s && b ? { space_id: s, board_id: b } : null
}

export function TemplatesPage({ workspace }: { workspace: number }) {
  const t = useT()
  const templates = useLoad(
    () => api<{ templates: AppTemplate[] }>(`${ws(workspace)}/app-templates`).then((r) => r.templates),
    [workspace],
  )
  const spaces = useLoad(() => api<{ spaces: Space[] }>(`${ws(workspace)}/spaces`).then((r) => r.spaces), [workspace])
  const [chosen, setChosen] = useState<string | null>(null)

  return (
    <div className="tt-section tt-templates">
      <div className="tt-pane-head">
        <h1>{t('apps.templates.title')}</h1>
        <Link className="tt-button" href={routePath({ workspace, section: 'apps' })}>
          {t('apps.all')}
        </Link>
      </div>
      <p className="tt-muted">{t('apps.templates.intro')}</p>
      {(templates.error || spaces.error) && (
        <p role="alert" className="tt-error">
          {templates.error ?? spaces.error}
        </p>
      )}
      {!templates.data && !templates.error && <p className="tt-muted">{t('app.loading')}</p>}
      <ul className="tt-template-list">
        {templates.data?.map((tpl) => (
          <li key={tpl.id} className="tt-template-card">
            <h2>{templateText(tpl, 'name', t)}</h2>
            <p>{templateText(tpl, 'description', t)}</p>
            <ul className="tt-template-facts tt-muted tt-small">
              <li>
                {t(tpl.recommended_space === 'private' ? 'apps.templates.forPrivate' : 'apps.templates.forShared')}
              </li>
              {tpl.captures.length > 0 && (
                <li>
                  {t('apps.templates.captures', {
                    kinds: tpl.captures.map((c) => t(`apps.templates.capture.${c}`)).join(', '),
                  })}
                </li>
              )}
              {tpl.needs_board && <li>{t('apps.templates.needsBoard')}</li>}
              {tpl.widgets.length > 0 && <li>{t('apps.templates.widgets')}</li>}
            </ul>
            {chosen === tpl.id ? (
              <InstallForm
                workspace={workspace}
                template={tpl}
                spaces={spaces.data ?? []}
                onCancel={() => setChosen(null)}
              />
            ) : (
              <button
                type="button"
                className="tt-button tt-primary"
                aria-label={t('apps.templates.useNamed', { name: templateText(tpl, 'name', t) })}
                onClick={() => setChosen(tpl.id)}
              >
                {t('apps.templates.use')}
              </button>
            )}
          </li>
        ))}
      </ul>
    </div>
  )
}

// Pick where it goes (private by default, a shared space only when asked for), and a board when the
// template needs one; installing makes the draft and opens it.
function InstallForm({
  workspace,
  template,
  spaces,
  onCancel,
}: {
  workspace: number
  template: AppTemplate
  spaces: Space[]
  onCancel: () => void
}) {
  const t = useT()
  const writable = spaces.filter((s) => s.access === 'write' && !s.archived_at)
  const mine = writable.find((s) => s.kind === 'private')
  const sharedSpaces = writable.filter((s) => s.kind === 'shared')
  const [name, setName] = useState(templateText(template, 'name', t))
  const [where, setWhere] = useState<'private' | 'shared'>('private')
  const [shared, setShared] = useState('')
  const [board, setBoard] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const boards = useWritableBoards(workspace, template.needs_board ? spaces : undefined)
  const space = where === 'private' ? mine : sharedSpaces.find((s) => String(s.id) === shared)
  const target = parseChoice(board)
  const ready = !!space && !!name.trim() && (!template.needs_board || !!target)
  const idPrefix = `tpl-${template.id}`

  const install = async (e: FormEvent) => {
    e.preventDefault()
    if (!space || busy) return
    setBusy(true)
    setError(null)
    try {
      const made = await api<App>(`${ws(workspace)}/spaces/${space.id}/app-templates/${template.id}/install`, {
        method: 'POST',
        body: { name: name.trim(), ...(template.needs_board && target ? { board: target } : {}) },
      })
      navigate(installedPath(workspace, space.id, made.id))
    } catch (err) {
      setError(appError(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form className="tt-template-form" onSubmit={install}>
      <label className="tt-field">
        <span>{t('apps.name')}</span>
        <input value={name} maxLength={100} required onChange={(e) => setName(e.target.value)} />
      </label>
      <fieldset className="tt-builder-where">
        <legend>{t('apps.builder.where')}</legend>
        <label className="tt-inline-field">
          <input
            type="radio"
            name={`${idPrefix}-where`}
            checked={where === 'private'}
            disabled={!mine}
            onChange={() => setWhere('private')}
          />
          {t('apps.builder.wherePrivate')}
        </label>
        <label className="tt-inline-field">
          <input
            type="radio"
            name={`${idPrefix}-where`}
            checked={where === 'shared'}
            disabled={sharedSpaces.length === 0}
            onChange={() => {
              setWhere('shared')
              setShared((s) => s || String(sharedSpaces[0]?.id ?? ''))
            }}
          />
          {t('apps.builder.whereShared')}
        </label>
        {where === 'shared' && (
          <label className="tt-inline-field">
            {t('apps.builder.sharedSpace')}
            <select value={shared} onChange={(e) => setShared(e.target.value)} required>
              {sharedSpaces.map((s) => (
                <option key={s.id} value={s.id}>
                  {spaceLabel(s)}
                </option>
              ))}
            </select>
          </label>
        )}
        <p className="tt-muted tt-small">
          {t(where === 'private' ? 'apps.builder.privateNote' : 'apps.builder.sharedNote')}
        </p>
        {where === 'shared' && template.roles && <RolesNote roles={template.roles} />}
        {where !== template.recommended_space && (
          <p className="tt-muted tt-small">
            {t(
              template.recommended_space === 'shared' ? 'apps.templates.betterShared' : 'apps.templates.betterPrivate',
            )}
          </p>
        )}
      </fieldset>
      {template.needs_board && (
        <div className="tt-field">
          <label htmlFor={`${idPrefix}-board`}>{t('apps.templates.board')}</label>
          <BoardSelect id={`${idPrefix}-board`} choices={boards.data ?? []} value={board} onChange={setBoard} />
          <span className="tt-muted tt-small">{t('apps.templates.boardNote')}</span>
          {boards.data && boards.data.length === 0 && (
            <span className="tt-muted tt-small">{t('apps.templates.noBoards')}</span>
          )}
        </div>
      )}
      <p className="tt-muted tt-small">{t('apps.templates.draftNote')}</p>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      <div className="tt-app-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || !ready}>
          {t('apps.templates.install')}
        </button>
        <button type="button" className="tt-button" disabled={busy} onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}

// What an install in a shared space gives people: the installer's roles, and the other members'.
function RolesNote({ roles }: { roles: NonNullable<AppTemplate['roles']> }) {
  const t = useT()
  const names = (list: AppRole[]) => list.map((r) => t(`apps.roles.${r}`)).join(', ')
  return (
    <p className="tt-small">
      {roles.members && roles.members.length > 0
        ? t('apps.templates.rolesShared', { installer: names(roles.installer), members: names(roles.members) })
        : t('apps.templates.rolesInstaller', { installer: names(roles.installer) })}
    </p>
  )
}

// Choose the board for effects that have none (a template's, a copy's whose boards its copier can't
// write): until then the draft can't be published.
export function BoardChooser({
  workspace,
  base,
  app,
  spaces,
  onChanged,
}: {
  workspace: number
  base: string
  app: App
  spaces: Space[] | undefined
  onChanged: () => void
}) {
  const t = useT()
  const boards = useWritableBoards(workspace, spaces)
  const [board, setBoard] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const target = parseChoice(board)
  const choose = async (e: FormEvent) => {
    e.preventDefault()
    if (!target) return
    setBusy(true)
    setError(null)
    try {
      await api<App>(`${base}/effect-board`, { method: 'POST', body: { version: app.version, ...target } })
      onChanged()
    } catch (err) {
      setError(appError(err, app.name))
      onChanged()
    } finally {
      setBusy(false)
    }
  }
  return (
    <form className="tt-template-form" onSubmit={choose} aria-label={t('apps.templates.board')}>
      <p>{t('apps.templates.boardNeeded')}</p>
      <div className="tt-field">
        <label htmlFor="tt-choose-board">{t('apps.templates.board')}</label>
        <BoardSelect id="tt-choose-board" choices={boards.data ?? []} value={board} onChange={setBoard} />
      </div>
      {boards.data && boards.data.length === 0 && <p className="tt-muted tt-small">{t('apps.templates.noBoards')}</p>}
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      <div className="tt-app-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || !target}>
          {t('apps.templates.useBoard')}
        </button>
      </div>
    </form>
  )
}

// The draft a template or a copy made: tried on sample data, given its board if it needs one, and
// published with the panel that says who will use it.
export function InstalledApp({ workspace, space, app }: { workspace: number; space: number; app: number }) {
  const t = useT()
  const base = `${ws(workspace)}/spaces/${space}/apps/${app}`
  const spaces = useLoad(() => api<{ spaces: Space[] }>(`${ws(workspace)}/spaces`).then((r) => r.spaces), [workspace])
  const info = useLoad(() => api<App>(base), [base])
  const where = spaces.data?.find((s) => s.id === space)
  const mayReview = !!info.data && (info.data.access.design || info.data.access.publish)

  return (
    <div className="tt-section tt-installed">
      <div className="tt-pane-head">
        <h1>{info.data?.name ?? t('apps.app')}</h1>
        <Link className="tt-button" href={routePath({ workspace, section: 'apps' })}>
          {t('apps.all')}
        </Link>
      </div>
      {(info.error || spaces.error) && (
        <p role="alert" className="tt-error">
          {info.error ?? spaces.error}
        </p>
      )}
      {!info.data && !info.error && <p className="tt-muted">{t('app.loading')}</p>}
      {info.data && (
        <>
          <p className="tt-muted">{t(info.data.live ? 'apps.templates.liveNote' : 'apps.templates.installedNote')}</p>
          {mayReview ? (
            <AppStage key={base} base={base} app={info.data} builder />
          ) : (
            <p className="tt-muted">{t('apps.templates.noReview')}</p>
          )}
          {info.data.needs_board && info.data.access.design && (
            <BoardChooser
              workspace={workspace}
              base={base}
              app={info.data}
              spaces={spaces.data}
              onChanged={info.reload}
            />
          )}
          {where && mayReview && (
            <PublishPanel
              base={base}
              spaceBase={`${ws(workspace)}/spaces/${space}`}
              app={info.data}
              space={where}
              onChanged={info.reload}
            />
          )}
        </>
      )}
    </div>
  )
}

// Duplicate as template: the app's definition, and nothing of its data, into a new app of a space
// the person may write (their private one by default).
export function DuplicateForm({
  workspace,
  space,
  app,
  spaces,
  onCancel,
}: {
  workspace: number
  space: Space
  app: App
  // The spaces the copy may go to: every one the user may write.
  spaces: Space[]
  onCancel: () => void
}) {
  const t = useT()
  const mine = spaces.find((s) => s.kind === 'private')
  const [name, setName] = useState(t('apps.duplicate.defaultName', { name: app.name }))
  const [to, setTo] = useState(String((mine ?? spaces[0])?.id ?? space.id))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const duplicate = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setError(null)
    try {
      const made = await api<App>(`${ws(workspace)}/spaces/${space.id}/apps/${app.id}/duplicate`, {
        method: 'POST',
        body: { name: name.trim(), space_id: Number(to) },
      })
      navigate(installedPath(workspace, made.space_id, made.id))
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form className="tt-template-form" onSubmit={duplicate} aria-label={t('apps.duplicate.title', { name: app.name })}>
      <p className="tt-small">{t('apps.duplicate.note')}</p>
      <label className="tt-field">
        <span>{t('apps.name')}</span>
        <input value={name} maxLength={100} required onChange={(e) => setName(e.target.value)} />
      </label>
      <label className="tt-field">
        <span>{t('apps.duplicate.space')}</span>
        <select value={to} onChange={(e) => setTo(e.target.value)} required>
          {spaces.map((s) => (
            <option key={s.id} value={s.id}>
              {spaceLabel(s)}
            </option>
          ))}
        </select>
      </label>
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
      <div className="tt-app-actions">
        <button type="submit" className="tt-button tt-primary" disabled={busy || !name.trim()}>
          {t('apps.duplicate.make')}
        </button>
        <button type="button" className="tt-button" disabled={busy} onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}
