// An app launched where people use it, with what they are looking at said plainly: the LIVE release,
// the one everybody uses, whose buttons change real data, or the PREVIEW of the draft, which isn't
// published and runs on sample data: its buttons work, and change nothing real. Anyone who may
// view the app sees the live release; those who may design or publish it can switch to the draft
// when it differs, and an app that was never published shows its draft alone (nobody else can
// launch it yet). In the builder the draft is where the person starts, and can always be switched
// to and from the live release.

import { useMemo, useState } from 'react'
import type { App } from '../api'
import { useT } from '../i18n'
import { AppView, httpTransport } from './AppView'

// reloadKey is changed by the page when the app's data changed behind the screen (a record was restored).
export function AppStage({
  base,
  app,
  reloadKey,
  builder,
}: {
  base: string
  app: App
  reloadKey?: number
  // In the builder: the draft is shown first, and the way to the live release is always there.
  builder?: boolean
}) {
  const t = useT()
  const [wantsDraft, setWantsDraft] = useState(builder ?? false)
  const reviewer = app.access.design || app.access.publish
  const live = app.live
  const draft = !!app.draft && reviewer && (wantsDraft || !live)
  const liveNumber = live?.number
  // A preview keeps its sample rows in its transport, so the transport stays while the screen is
  // in use; a new draft (the app's version moved) is a new preview.
  const version = draft ? app.version : 0
  // biome-ignore lint/correctness/useExhaustiveDependencies: a new version of the draft is a new preview
  const transport = useMemo(() => httpTransport(base, draft ? 'draft' : liveNumber), [base, draft, liveNumber, version])

  if (!live && !draft) {
    return <p className="tt-muted tt-small">{t('apps.stage.notPublished')}</p>
  }
  const needs = (draft ? app.draft : live) ?? undefined
  const toggle = live && reviewer && (builder || app.unpublished_changes)
  return (
    <div className="tt-stage" data-stage={draft ? 'draft' : 'live'}>
      <div className="tt-stage-head">
        <span className="tt-stage-label" data-stage={draft ? 'draft' : 'live'}>
          {draft ? t('apps.stage.draft') : t('apps.stage.live', { number: live?.number ?? 0 })}
        </span>
        {toggle && (
          <button type="button" className="tt-button tt-small" onClick={() => setWantsDraft((d) => !d)}>
            {draft ? t('apps.stage.showLive') : t('apps.stage.showDraft')}
          </button>
        )}
      </div>
      {draft && <p className="tt-muted tt-small">{t('apps.stage.draftNote')}</p>}
      {!draft && live && reviewer && app.unpublished_changes && (
        <p className="tt-muted tt-small">{t('apps.stage.unpublishedNote')}</p>
      )}
      {!draft && live && builder && <p className="tt-muted tt-small">{t('apps.stage.liveNote')}</p>}
      <AppView
        key={draft ? `draft-${version}` : live?.number}
        surfaceId={`app-${app.id}`}
        label={app.name}
        release={needs}
        reloadKey={reloadKey}
        transport={transport}
      />
    </div>
  )
}
