// Personal apps: the apps of every space the user can read, by space, and one launched app
// with its data. Apps are built by asking the agent; here they are launched, renamed, taken
// offline and restored, published, rolled back and shared by moving them to a shared space. An app
// has a draft, which the agent and builders change, and releases: people use the live release, and
// someone who may publish the app makes the draft the next one, acknowledging the changes that
// stored rows are affected by, or rolls back to an earlier release. A space's editors change its
// apps and its viewers launch them; roles in an app narrow that further (a user who may only submit
// records can't rename, publish or move it), and the server's answer for the app, `access`, says
// what the caller may do.

import { type FormEvent, useState } from 'react'
import { ApiError, type App, api, errorText, type SchemaChange, type Space, ws } from '../api'
import type { EventSourceFactory } from '../conversations/useConversationStream'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { Link, routePath } from '../router'
import { spaceLabel } from '../spaces'
import { AppBuilder, lastBuilderConversation } from './AppBuilder'
import { appError, MigrationNotice, publishDraft } from './AppPublish'
import { AppRecords } from './AppRecords'
import { AppReleases } from './AppReleases'
import { AppRoles } from './AppRoles'
import { AppStage } from './AppStage'
import { appPath, builderPath, parseBuilder, parseItem } from './route'

const appsBase = (workspace: number, space: number) => `${ws(workspace)}/spaces/${space}/apps`

export function AppsPage({
  workspace,
  open,
  streams,
}: {
  workspace: number
  open?: string
  // The builder's conversation stream: a test's own.
  streams?: EventSourceFactory
}) {
  const spaces = useLoad(() => api<{ spaces: Space[] }>(`${ws(workspace)}/spaces`).then((r) => r.spaces), [workspace])
  // Bumped when an app moves, so every space's list shows where it is now.
  const [moves, setMoves] = useState(0)
  const t = useT()
  const continuing = lastBuilderConversation(workspace)
  const builder = parseBuilder(open)
  if (builder) return <AppBuilder workspace={workspace} route={builder} streams={streams} />
  const launched = parseItem(open)
  if (launched) {
    const space = spaces.data?.find((s) => s.id === launched.space)
    return (
      <LaunchedApp
        workspace={workspace}
        space={launched.space}
        app={launched.app}
        writable={space?.access === 'write'}
        screen={launched.screen}
      />
    )
  }
  return (
    <div className="tt-section">
      <div className="tt-pane-head">
        <h1>{t('shell.sections.apps')}</h1>
        <Link className="tt-button tt-primary" href={builderPath(workspace)}>
          {t('apps.builder.build')}
        </Link>
        {continuing && (
          <Link className="tt-button" href={builderPath(workspace, { kind: 'conversation', conversation: continuing })}>
            {t('apps.builder.continue')}
          </Link>
        )}
      </div>
      <p className="tt-muted">{t('apps.intro')}</p>
      {spaces.error && (
        <p role="alert" className="tt-error">
          {spaces.error}
        </p>
      )}
      {!spaces.data && !spaces.error && <p className="tt-muted">{t('app.loading')}</p>}
      {spaces.data?.length === 0 && <p className="tt-muted">{t('apps.noSpaces')}</p>}
      {spaces.data?.map((s) => (
        <SpaceApps
          key={s.id}
          workspace={workspace}
          space={s}
          targets={spaces.data?.filter((t) => t.id !== s.id && t.access === 'write' && !t.archived_at) ?? []}
          moves={moves}
          moved={() => setMoves((n) => n + 1)}
        />
      ))}
    </div>
  )
}

function SpaceApps({
  workspace,
  space,
  targets,
  moves,
  moved,
}: {
  workspace: number
  space: Space
  targets: Space[]
  moves: number
  moved: () => void
}) {
  const base = appsBase(workspace, space.id)
  const apps = useLoad(() => api<{ apps: App[] }>(`${base}?archived=true`).then((r) => r.apps), [base, moves])
  const [error, setError] = useState<string | null>(null)
  const writable = space.access === 'write'
  const t = useT()

  // change applies a change to an app, then shows the list as it is now: on a stale version
  // too, so the next try is made against the current one.
  const change = async (app: App, body: { name?: string; archived?: boolean }) => {
    try {
      await api<App>(`${base}/${app.id}`, { method: 'PATCH', body: { version: app.version, ...body } })
      setError(null)
    } catch (e) {
      setError(e instanceof ApiError && e.status === 409 ? t('apps.changed', { name: app.name }) : errorText(e))
    }
    apps.reload()
  }
  // publish makes the app's draft its live release. It answers the schema changes the server wants
  // acknowledged first (nothing is published then), or null: published, or failed (the error is
  // shown, and the list reloaded, so the next try is made against the current version).
  const publish = async (app: App, acknowledge: SchemaChange[]): Promise<SchemaChange[] | null> => {
    try {
      const result = await publishDraft(`${base}/${app.id}`, app, acknowledge)
      if ('changes' in result) {
        setError(null)
        return result.changes
      }
      setError(null)
    } catch (e) {
      setError(
        e instanceof ApiError && e.code === 'app_changed'
          ? t('apps.changed', { name: app.name })
          : appError(e, app.name),
      )
    }
    apps.reload()
    return null
  }
  // move moves an app, with its rows, to another space the user may write.
  const move = async (app: App, to: number) => {
    try {
      await api<App>(`${base}/${app.id}/move`, { method: 'POST', body: { version: app.version, space_id: to } })
      setError(null)
      moved()
    } catch (e) {
      setError(errorText(e))
      apps.reload()
    }
  }

  return (
    <section className="tt-app-space" aria-label={spaceLabel(space)}>
      <h2>
        {spaceLabel(space)}
        {!writable && <span className="tt-muted tt-small"> {t('apps.readOnly')}</span>}
      </h2>
      {(apps.error || error) && (
        <p role="alert" className="tt-error">
          {error ?? apps.error}
        </p>
      )}
      {!apps.data && !apps.error && <p className="tt-muted">{t('app.loading')}</p>}
      {apps.data?.length === 0 && <p className="tt-muted">{t('apps.none')}</p>}
      {apps.data && apps.data.length > 0 && (
        <ul className="tt-app-list">
          {apps.data.map((a) => (
            <AppRow
              key={a.id}
              workspace={workspace}
              space={space}
              app={a}
              writable={writable}
              targets={targets}
              spaceBase={`${ws(workspace)}/spaces/${space.id}`}
              change={change}
              move={move}
              publish={publish}
              reload={apps.reload}
            />
          ))}
        </ul>
      )}
    </section>
  )
}

function AppRow({
  workspace,
  space,
  app,
  writable,
  targets,
  spaceBase,
  change,
  move,
  publish,
  reload,
}: {
  workspace: number
  space: Space
  app: App
  writable: boolean
  targets: Space[]
  spaceBase: string
  change: (app: App, body: { name?: string; archived?: boolean }) => Promise<void>
  move: (app: App, to: number) => Promise<void>
  publish: (app: App, acknowledge: SchemaChange[]) => Promise<SchemaChange[] | null>
  reload: () => void
}) {
  const [renaming, setRenaming] = useState(false)
  const [to, setTo] = useState('')
  const [name, setName] = useState(app.name)
  const [busy, setBusy] = useState(false)
  const [rolesOpen, setRolesOpen] = useState(false)
  const [releasesOpen, setReleasesOpen] = useState(false)
  // The schema changes a publish waits to have acknowledged.
  const [migration, setMigration] = useState<SchemaChange[] | null>(null)
  const offline = app.archived_at !== null
  // Those who may design or publish the app see what isn't live: its draft and its releases.
  const reviewer = writable && (app.access.design || app.access.publish)
  const t = useT()
  const runPublish = async (acknowledge: SchemaChange[]) => {
    setBusy(true)
    const needed = await publish(app, acknowledge)
    setBusy(false)
    setMigration(needed)
  }
  const run = async (body: { name?: string; archived?: boolean }) => {
    setBusy(true)
    await change(app, body)
    setBusy(false)
  }
  const rename = async (e: FormEvent) => {
    e.preventDefault()
    await run({ name })
    setRenaming(false)
  }

  return (
    <li className="tt-app-row" data-offline={offline || undefined}>
      {renaming ? (
        <form className="tt-app-rename" onSubmit={rename}>
          <label className="tt-inline-field">
            {t('apps.name')}
            <input value={name} onChange={(e) => setName(e.target.value)} maxLength={100} required />
          </label>
          <button type="submit" className="tt-button tt-primary" disabled={busy}>
            {t('apps.save')}
          </button>
          <button
            type="button"
            className="tt-button"
            onClick={() => {
              setName(app.name)
              setRenaming(false)
            }}
          >
            {t('common.cancel')}
          </button>
        </form>
      ) : (
        <div className="tt-app-name">
          <span className="tt-list-title">{app.name}</span>
          <span className="tt-muted tt-small">
            {[t('apps.rows', { count: app.rows }), ...(offline ? [t('apps.offline')] : [])].join(' · ')}
          </span>
          <span className="tt-app-status">
            {app.live ? (
              <span className="tt-stage-label" data-stage="live">
                {t('apps.stage.live', { number: app.live.number })}
              </span>
            ) : (
              <span className="tt-stage-label" data-stage="draft">
                {t('apps.notPublished')}
              </span>
            )}
            {app.live && app.unpublished_changes && (
              <span className="tt-stage-label" data-stage="draft">
                {t('apps.unpublishedChanges')}
              </span>
            )}
          </span>
        </div>
      )}
      {!renaming && (
        <div className="tt-app-actions">
          {!offline && app.live && (
            <Link
              className="tt-button tt-primary"
              href={appPath(workspace, space.id, app.id)}
              aria-label={t('apps.launchNamed', { name: app.name })}
            >
              {t('apps.launch')}
            </Link>
          )}
          {!offline && !app.live && reviewer && (
            <Link
              className="tt-button"
              href={appPath(workspace, space.id, app.id)}
              aria-label={t('apps.previewDraftNamed', { name: app.name })}
            >
              {t('apps.previewDraft')}
            </Link>
          )}
          {writable && app.access.publish && app.unpublished_changes && (
            <button
              type="button"
              className="tt-button tt-primary"
              disabled={busy || migration !== null}
              onClick={() => runPublish([])}
              aria-label={t('apps.publishNamed', { name: app.name })}
            >
              {t('apps.publish')}
            </button>
          )}
          {reviewer && app.live && (
            <button
              type="button"
              className="tt-button"
              aria-expanded={releasesOpen}
              onClick={() => setReleasesOpen((o) => !o)}
              aria-label={t('apps.releases.openNamed', { name: app.name })}
            >
              {t('apps.releases.open')}
            </button>
          )}
          {!offline && writable && app.access.design && (
            <Link
              className="tt-button"
              href={builderPath(workspace, { kind: 'edit', space: space.id, app: app.id })}
              aria-label={t('apps.builder.editNamed', { name: app.name })}
            >
              {t('apps.builder.edit')}
            </Link>
          )}
          {writable && app.access.design && (
            <button type="button" className="tt-button" disabled={busy} onClick={() => setRenaming(true)}>
              {t('apps.rename')}
            </button>
          )}
          {writable && app.access.publish && (
            <button
              type="button"
              className="tt-button"
              disabled={busy}
              onClick={() => run({ archived: !offline })}
              aria-label={t(offline ? 'apps.restoreNamed' : 'apps.takeOfflineNamed', { name: app.name })}
            >
              {offline ? t('apps.restore') : t('apps.takeOffline')}
            </button>
          )}
          {writable && app.access.manage_roles && space.kind === 'shared' && (
            <button
              type="button"
              className="tt-button"
              aria-expanded={rolesOpen}
              onClick={() => setRolesOpen((o) => !o)}
              aria-label={t('apps.roles.openNamed', { name: app.name })}
            >
              {t('apps.roles.open')}
            </button>
          )}
          {writable && app.access.design && targets.length > 0 && (
            <form
              className="tt-app-move"
              onSubmit={async (e) => {
                e.preventDefault()
                setBusy(true)
                await move(app, Number(to))
                setBusy(false)
              }}
            >
              <select
                aria-label={t('apps.moveNamed', { name: app.name })}
                value={to}
                onChange={(e) => setTo(e.target.value)}
                required
              >
                <option value="">{t('apps.moveTo')}</option>
                {targets.map((t) => (
                  <option key={t.id} value={t.id}>
                    {spaceLabel(t)}
                  </option>
                ))}
              </select>
              <button type="submit" className="tt-button" disabled={busy || !to}>
                {t('apps.move')}
              </button>
            </form>
          )}
        </div>
      )}
      {migration && (
        <MigrationNotice
          changes={migration}
          busy={busy}
          onPublish={() => runPublish(migration)}
          onCancel={() => setMigration(null)}
        />
      )}
      {releasesOpen && (
        <AppReleases
          base={`${spaceBase}/apps/${app.id}`}
          app={app}
          canRollBack={writable && app.access.publish}
          changed={reload}
        />
      )}
      {rolesOpen && (
        <AppRoles base={`${spaceBase}/apps/${app.id}`} spaceBase={spaceBase} onClose={() => setRolesOpen(false)} />
      )}
    </li>
  )
}

function LaunchedApp({
  workspace,
  space,
  app,
  writable,
  screen,
}: {
  workspace: number
  space: number
  app: number
  writable: boolean
  screen?: string
}) {
  const base = `${appsBase(workspace, space)}/${app}`
  const info = useLoad(() => api<App>(base), [base])
  // Bumped when a record is restored, so the screen is loaded again with it.
  const [restored, setRestored] = useState(0)
  const back = routePath({ workspace, section: 'apps' })
  const t = useT()
  const restore = async () => {
    if (!info.data) return
    try {
      await api<App>(base, { method: 'PATCH', body: { version: info.data.version, archived: false } })
    } finally {
      info.reload()
    }
  }

  return (
    <div className="tt-section">
      <div className="tt-pane-head">
        <h1>{info.data?.name ?? t('apps.app')}</h1>
        {info.data && !info.data.archived_at && writable && info.data.access.design && (
          <Link
            className="tt-button"
            href={builderPath(workspace, { kind: 'edit', space, app })}
            aria-label={t('apps.builder.editNamed', { name: info.data.name })}
          >
            {t('apps.builder.edit')}
          </Link>
        )}
        <Link className="tt-button" href={back}>
          {t('apps.all')}
        </Link>
      </div>
      {info.error && (
        <p role="alert" className="tt-error">
          {info.error}
        </p>
      )}
      {!info.data && !info.error && <p className="tt-muted">{t('app.loading')}</p>}
      {info.data?.archived_at && (
        <div className="tt-app-offline">
          <p>{t('apps.offlineNotice')}</p>
          {writable && info.data.access.publish && (
            <button type="button" className="tt-button tt-primary" onClick={restore}>
              {t('apps.restore')}
            </button>
          )}
        </div>
      )}
      {info.data && !info.data.archived_at && (
        <>
          {!writable && <p className="tt-muted tt-small">{t('apps.viewOnly')}</p>}
          {writable && !info.data.access.submit && info.data.access.modify_records === 'none' && (
            <p className="tt-muted tt-small">{t('apps.noDataChanges')}</p>
          )}
          {info.data.access.read_records === 'own' && <p className="tt-muted tt-small">{t('apps.ownRecords')}</p>}
          <AppStage key={screen ?? ''} base={base} app={info.data} reloadKey={restored} screen={screen} />
          <AppRecords
            base={base}
            appId={app}
            access={info.data.access}
            writable={writable}
            definition={info.data.live?.definition}
            release={info.data.live?.number}
            onRestored={() => setRestored((n) => n + 1)}
          />
        </>
      )}
    </div>
  )
}
