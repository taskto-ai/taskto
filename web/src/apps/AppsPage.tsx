// Personal apps: the apps of every space the user can read, by space, and one launched app
// with its data. Apps are built by asking the agent; here they are launched, renamed, taken
// offline and restored, published, rolled back and shared by moving them to a shared space. An app
// has a draft, which the agent and builders change, and releases: people use the live release, and
// someone who may publish the app makes the draft the next one, acknowledging the changes that
// stored rows are affected by, or rolls back to an earlier release. A space's editors change its
// apps and its viewers launch them; roles in an app narrow that further (a user who may only submit
// records can't rename, publish or move it), and the server's answer for the app, `access`, says
// what the caller may do.

import { useState } from 'react'
import { ApiError, type App, api, errorText, type Space, ws } from '../api'
import type { EventSourceFactory } from '../conversations/useConversationStream'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { spaceLabel } from '../spaces'
import { ButtonLink } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { Heading } from '../ui/Heading'
import { modalOpen, onModalChange } from '../ui/layers'
import { Notice } from '../ui/Notice'
import { Page } from '../ui/Page'
import { PageHeader } from '../ui/PageHeader'
import { Panel } from '../ui/Panel'
import { Rows } from '../ui/Rows'
import { AppBuilder, lastBuilderConversation } from './AppBuilder'
import { AppRow } from './AppRow'
import { LoadingRows } from './AppsLoading'
import { InstalledApp, TemplatesPage } from './AppTemplates'
import { LaunchedApp } from './LaunchedApp'
import { builderPath, isTemplates, parseBuilder, parseInstalled, parseItem, templatesPath } from './route'
import './Apps.css'

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
    <Page width="list">
      <PageHeader
        title={t('shell.sections.apps')}
        help={t('apps.intro')}
        actions={
          <>
            {continuing && (
              <ButtonLink
                variant="ghost"
                href={builderPath(workspace, { kind: 'conversation', conversation: continuing })}
              >
                {t('apps.builder.continue')}
              </ButtonLink>
            )}
            <ButtonLink href={templatesPath(workspace)}>{t('apps.templates.start')}</ButtonLink>
            <ButtonLink variant="primary" icon="plus" href={builderPath(workspace)}>
              {t('apps.builder.build')}
            </ButtonLink>
          </>
        }
      />
      {spaces.error && <Notice tone="danger">{spaces.error}</Notice>}
      {!spaces.data && !spaces.error && <LoadingRows count={3} />}
      {spaces.data?.length === 0 && (
        <EmptyState title={t('apps.noSpaces')}>
          <p>{t('apps.noSpacesHelp')}</p>
        </EmptyState>
      )}
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
    </Page>
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
  const headingId = `apps-space-${space.id}`

  // change applies a change to an app, then shows the list as it is now: on a stale version
  // too, so the next try is made against the current one. It answers what went wrong, if anything,
  // for the row to show where the person is: in the dialog that asked, or on the page.
  const change = async (app: App, body: { name?: string; archived?: boolean }) => {
    let failed: string | null = null
    try {
      await api<App>(`${base}/${app.id}`, { method: 'PATCH', body: { version: app.version, ...body } })
      setError(null)
    } catch (e) {
      failed = e instanceof ApiError && e.status === 409 ? t('apps.changed', { name: app.name }) : errorText(e)
    }
    apps.reload()
    return failed
  }
  // move moves an app, with its rows, to another space the user may write. The app's row is gone
  // then, so the focus goes to this space's heading.
  const move = async (app: App, to: number) => {
    try {
      await api<App>(`${base}/${app.id}/move`, { method: 'POST', body: { version: app.version, space_id: to } })
      setError(null)
      focusHeadingWhenUncovered(headingId)
      moved()
      return null
    } catch (e) {
      apps.reload()
      return errorText(e)
    }
  }

  return (
    <section className="apps-group" aria-labelledby={headingId}>
      <header className="apps-group-head">
        <Heading level={2} id={headingId} tabIndex={-1}>
          {spaceLabel(space)}
        </Heading>
        <span className="apps-note">
          {[
            ...(apps.data ? [t('apps.count', { count: apps.data.length })] : []),
            ...(space.kind === 'shared' ? [t('apps.sharedSpace')] : []),
            ...(!writable ? [t('apps.readOnlyShort')] : []),
          ].join(' · ')}
        </span>
      </header>
      {(apps.error || error) && <Notice tone="danger">{error ?? apps.error}</Notice>}
      {!apps.data && !apps.error && <LoadingRows count={2} />}
      {apps.data?.length === 0 && <NoApps workspace={workspace} space={space} />}
      {apps.data && apps.data.length > 0 && (
        <Panel>
          <Rows label={spaceLabel(space)}>
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
                report={setError}
                reload={apps.reload}
              />
            ))}
          </Rows>
        </Panel>
      )}
    </section>
  )
}

// After a move the app's row is gone: the focus goes to the heading of the space it left. The
// move's dialog is still open now, and everything under a modal dialog is inert, so the heading
// takes the focus once no modal is open: just after, since a closing dialog gives the focus back
// to what opened it first.
function focusHeadingWhenUncovered(id: string) {
  const focus = () => document.getElementById(id)?.focus()
  if (!modalOpen()) return focus()
  const stop = onModalChange(() => {
    if (modalOpen()) return
    stop()
    queueMicrotask(focus)
  })
}

// A space with no apps: what apps there are for, and the two ways to start one (only where the
// person may add one).
function NoApps({ workspace, space }: { workspace: number; space: Space }) {
  const t = useT()
  const writable = space.access === 'write' && !space.archived_at
  const name = spaceLabel(space)
  return (
    <EmptyState
      title={t('apps.empty.title', { space: name })}
      actions={
        writable && (
          <>
            <ButtonLink size="sm" href={templatesPath(workspace)}>
              {t('apps.templates.start')}
            </ButtonLink>
            <ButtonLink size="sm" href={builderPath(workspace)}>
              {t('apps.builder.build')}
            </ButtonLink>
          </>
        )
      }
    >
      <p>
        {t(
          space.kind === 'private'
            ? 'apps.empty.private'
            : writable
              ? 'apps.empty.shared'
              : 'apps.empty.sharedReadOnly',
        )}
      </p>
    </EmptyState>
  )
}
