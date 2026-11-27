import { type ComponentProps, useMemo, useState } from 'react'
import type { App } from '../api'
import { useT } from '../i18n'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { LazyAppView as AppView } from './LazyAppView'
import { httpTransport } from './transport'
import './Apps.css'

export function AppStage({
  base,
  app,
  reloadKey,
  screen,
  onAction,
  builder,
}: {
  base: string
  app: App
  reloadKey?: number
  screen?: string
  onAction?: () => void
  builder?: boolean
}) {
  const t = useT()
  const [wantsDraft, setWantsDraft] = useState(builder ?? false)
  const reviewer = app.access.design || app.access.publish
  const live = app.live
  const unpublished = !live || app.unpublished_changes
  if (live && !unpublished && wantsDraft) setWantsDraft(false)
  const draft = !!app.draft && reviewer && unpublished && (wantsDraft || !live)
  const liveNumber = live?.number
  const version = app.version
  const liveTransport = useMemo(() => httpTransport(base, liveNumber, { screen }), [base, liveNumber, screen])

  if (!live && !draft) {
    return <p className="apps-note">{t('apps.stage.notPublished')}</p>
  }
  const toggle = live && reviewer && app.unpublished_changes
  return (
    <div className="apps-stage" data-stage={draft ? 'draft' : 'live'}>
      <div className="apps-stage-head">
        <Badge tone={draft ? 'warning' : 'success'} dot>
          {draft ? t('apps.stage.draft') : t('apps.stage.live', { number: live?.number ?? 0 })}
        </Badge>
        {toggle && (
          <Button size="sm" onClick={() => setWantsDraft((d) => !d)}>
            {draft ? t('apps.stage.showLive') : t('apps.stage.showDraft')}
          </Button>
        )}
      </div>
      {draft && <p className="apps-note">{t('apps.stage.draftNote')}</p>}
      {!draft && live && reviewer && app.unpublished_changes && (
        <p className="apps-note" role={builder ? 'status' : undefined}>
          {t(builder ? 'apps.stage.newDraft' : 'apps.stage.unpublishedNote')}
        </p>
      )}
      {!draft && live && builder && (
        <p className="apps-note">
          {t(reviewer && !app.unpublished_changes ? 'apps.stage.nothingUnpublished' : 'apps.stage.liveNote')}
        </p>
      )}
      {live && (
        <div hidden={draft}>
          <AppView
            key={live.number}
            surfaceId={`app-${app.id}`}
            label={app.name}
            release={live}
            reloadKey={reloadKey}
            onAction={onAction}
            transport={liveTransport}
          />
        </div>
      )}
      {draft && (
        <DraftView
          key={`draft-${version}`}
          base={base}
          screen={screen}
          surfaceId={`app-${app.id}`}
          label={app.name}
          release={app.draft ?? undefined}
          reloadKey={reloadKey}
          onAction={onAction}
        />
      )}
    </div>
  )
}

function DraftView({
  base,
  screen,
  ...view
}: Omit<ComponentProps<typeof AppView>, 'transport'> & { base: string; screen?: string }) {
  const transport = useMemo(() => httpTransport(base, 'draft', { screen }), [base, screen])
  return <AppView {...view} transport={transport} />
}
