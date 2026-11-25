// An app launched where people use it, with what they are looking at said plainly: the LIVE release,
// the one everybody uses, whose buttons change real data, or the PREVIEW of the draft, which isn't
// published and runs on sample data: its buttons work, and change nothing real. Anyone who may
// view the app sees the live release; those who may design or publish it can switch to the draft
// when it differs, and an app that was never published shows its draft alone (nobody else can
// launch it yet). In the builder the draft is where the person starts, and can be switched to and
// from the live release, while there is something unpublished: a draft that is the live release
// is nothing to preview, so the live app is shown.

import { useMemo, useState } from 'react'
import type { App } from '../api'
import { useT } from '../i18n'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { AppView, httpTransport } from './AppView'
import './Apps.css'

// reloadKey is changed by the page when the app's data changed behind the screen (a record was restored);
// screen is the one an app with screens opens on instead of its entry (a pin's); onAction is called
// after one of the app's buttons ran.
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
  // In the builder: the draft is shown first (while it has something the live release hasn't), and the
  // way to the live release is there.
  builder?: boolean
}) {
  const t = useT()
  const [wantsDraft, setWantsDraft] = useState(builder ?? false)
  const reviewer = app.access.design || app.access.publish
  const live = app.live
  // The draft is only worth showing while it isn't the live release (the server says so, to those
  // who review): once it is published there is nothing left to preview, and the live app is the app.
  const unpublished = !live || app.unpublished_changes
  // Nothing unpublished: the live app is shown, and a draft the agent saves next doesn't take the screen
  // from the person using it (what they typed into the live form would be lost); they switch when ready.
  if (live && !unpublished && wantsDraft) setWantsDraft(false)
  const draft = !!app.draft && reviewer && unpublished && (wantsDraft || !live)
  const liveNumber = live?.number
  // A preview keeps its sample rows in its transport, so the transport stays while the screen is
  // in use; a new draft (the app's version moved) is a new preview.
  const version = app.version
  const liveTransport = useMemo(() => httpTransport(base, liveNumber, { screen }), [base, liveNumber, screen])
  // biome-ignore lint/correctness/useExhaustiveDependencies: a new version of the draft is a new preview
  const draftTransport = useMemo(() => httpTransport(base, 'draft', { screen }), [base, version, screen])

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
      {/* The live screen stays drawn while the draft is looked at, so what was typed into it is still there. */}
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
        <AppView
          key={`draft-${version}`}
          surfaceId={`app-${app.id}`}
          label={app.name}
          release={app.draft ?? undefined}
          reloadKey={reloadKey}
          onAction={onAction}
          transport={draftTransport}
        />
      )}
    </div>
  )
}
