// An app launched where people use it, with what they are looking at said plainly: the LIVE release,
// the one everybody uses, or the DRAFT, a preview of what builders are changing that isn't
// published and whose buttons don't run. Anyone who may view the app sees the live release; those
// who may design or publish it can switch to the draft when it differs, and an app that was never
// published shows its draft alone (nobody else can launch it yet).

import { useState } from 'react'
import type { App } from '../api'
import { useT } from '../i18n'
import { AppView, httpTransport } from './AppView'

// reloadKey is changed by the page when the app's data changed behind the screen (a record was restored);
// screen is the one an app with screens opens on instead of its entry (a pin's).
export function AppStage({
  base,
  app,
  reloadKey,
  screen,
}: {
  base: string
  app: App
  reloadKey?: number
  screen?: string
}) {
  const t = useT()
  const [wantsDraft, setWantsDraft] = useState(false)
  const reviewer = app.access.design || app.access.publish
  const live = app.live
  const draft = !!app.draft && reviewer && (wantsDraft || !live)

  if (!live && !draft) {
    return <p className="tt-muted tt-small">{t('apps.stage.notPublished')}</p>
  }
  const needs = (draft ? app.draft : live) ?? undefined
  return (
    <div className="tt-stage">
      <div className="tt-stage-head">
        <span className="tt-stage-label" data-stage={draft ? 'draft' : 'live'}>
          {draft ? t('apps.stage.draft') : t('apps.stage.live', { number: live?.number ?? 0 })}
        </span>
        {live && reviewer && app.unpublished_changes && (
          <button type="button" className="tt-button tt-small" onClick={() => setWantsDraft((d) => !d)}>
            {draft ? t('apps.stage.showLive') : t('apps.stage.showDraft')}
          </button>
        )}
      </div>
      {draft && <p className="tt-muted tt-small">{t('apps.stage.draftNote')}</p>}
      {!draft && live && reviewer && app.unpublished_changes && (
        <p className="tt-muted tt-small">{t('apps.stage.unpublishedNote')}</p>
      )}
      <AppView
        key={draft ? 'draft' : live?.number}
        surfaceId={`app-${app.id}`}
        label={app.name}
        release={needs}
        reloadKey={reloadKey}
        transport={httpTransport(base, draft ? 'draft' : live?.number, { screen })}
      />
    </div>
  )
}
