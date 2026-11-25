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
import { ApiError, type App, api, errorText, type Space, ws } from '../api'
import type { EventSourceFactory } from '../conversations/useConversationStream'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { Link } from '../router'
import { spaceLabel } from '../spaces'
import { AppBuilder, lastBuilderConversation } from './AppBuilder'
import { appError, PublishPanel } from './AppPublish'
import { AppReleases } from './AppReleases'
import { AppRoles } from './AppRoles'
import { DuplicateForm, InstalledApp, TemplatesPage } from './AppTemplates'
import { LaunchedApp } from './LaunchedApp'
import { appPath, builderPath, isTemplates, parseBuilder, parseInstalled, parseItem, templatesPath } from './route'

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
  if (isTemplates(open)) return <TemplatesPage workspace={workspace} />
  const installed = parseInstalled(open)
  if (installed) return <InstalledApp workspace={workspace} space={installed.space} app={installed.app} />
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
        spaceInfo={space}
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
        <Link className="tt-button" href={templatesPath(workspace)}>
          {t('apps.templates.start')}
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
              copyTo={writable ? [space, ...targets] : targets}
              spaceBase={`${ws(workspace)}/spaces/${space.id}`}
              change={change}
              move={move}
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
  copyTo,
  spaceBase,
  change,
  move,
  reload,
}: {
  workspace: number
  space: Space
  app: App
  writable: boolean
  targets: Space[]
  // Where a copy of the app may go: every space the user may write.
  copyTo: Space[]
  spaceBase: string
  change: (app: App, body: { name?: string; archived?: boolean }) => Promise<void>
  move: (app: App, to: number) => Promise<void>
  reload: () => void
}) {
  const [renaming, setRenaming] = useState(false)
  const [to, setTo] = useState('')
  const [name, setName] = useState(app.name)
  const [busy, setBusy] = useState(false)
  const [rolesOpen, setRolesOpen] = useState(false)
  const [releasesOpen, setReleasesOpen] = useState(false)
  const [duplicating, setDuplicating] = useState(false)
  // The app as its publisher reviews it before publishing: with its draft and where its effects go,
  // which the list doesn't carry. Publishing is only from there, so what the draft does is seen first.
  const [reviewing, setReviewing] = useState<App | null>(null)
  const [reviewError, setReviewError] = useState<string | null>(null)
  const offline = app.archived_at !== null
  // Those who may design or publish the app see what isn't live: its draft and its releases.
  const reviewer = writable && (app.access.design || app.access.publish)
  const t = useT()
  const appBase = `${spaceBase}/apps/${app.id}`
  const review = async () => {
    setBusy(true)
    setReviewError(null)
    try {
      setReviewing(await api<App>(appBase))
    } catch (e) {
      setReviewError(appError(e, app.name))
    }
    setBusy(false)
  }
  // The panel published the app, or found that it changed: the list and the panel read it again.
  const reviewed = async () => {
    reload()
    try {
      setReviewing(await api<App>(appBase))
    } catch (e) {
      setReviewing(null)
      setReviewError(appError(e, app.name))
    }
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
              disabled={busy || reviewing !== null}
              onClick={review}
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
          {copyTo.length > 0 && (
            <button
              type="button"
              className="tt-button"
              aria-expanded={duplicating}
              onClick={() => setDuplicating((d) => !d)}
              aria-label={t('apps.duplicate.openNamed', { name: app.name })}
            >
              {t('apps.duplicate.open')}
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
      {reviewError && (
        <p role="alert" className="tt-error">
          {reviewError}
        </p>
      )}
      {reviewing && (
        <>
          <PublishPanel base={appBase} spaceBase={spaceBase} app={reviewing} space={space} onChanged={reviewed} />
          <button
            type="button"
            className="tt-button"
            aria-label={t('apps.closeReviewNamed', { name: app.name })}
            onClick={() => setReviewing(null)}
          >
            {t('apps.closeReview')}
          </button>
        </>
      )}
      {releasesOpen && (
        <AppReleases
          base={`${spaceBase}/apps/${app.id}`}
          app={app}
          canRollBack={writable && app.access.publish}
          changed={reload}
        />
      )}
      {duplicating && (
        <DuplicateForm
          workspace={workspace}
          space={space}
          app={app}
          spaces={copyTo}
          onCancel={() => setDuplicating(false)}
        />
      )}
      {rolesOpen && (
        <AppRoles base={`${spaceBase}/apps/${app.id}`} spaceBase={spaceBase} onClose={() => setRolesOpen(false)} />
      )}
    </li>
  )
}
