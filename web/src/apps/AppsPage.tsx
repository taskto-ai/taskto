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
  streams?: EventSourceFactory
}) {
  const spaces = useLoad(() => api<{ spaces: Space[] }>(`${ws(workspace)}/spaces`).then((r) => r.spaces), [workspace])
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

  const change = async (app: App, body: { name?: string; icon?: string; archived?: boolean }) => {
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

function focusHeadingWhenUncovered(id: string) {
  const focus = () => document.getElementById(id)?.focus()
  if (!modalOpen()) return focus()
  const stop = onModalChange(() => {
    if (modalOpen()) return
    stop()
    queueMicrotask(focus)
  })
}

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
