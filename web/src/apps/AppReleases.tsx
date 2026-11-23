// An app's release history: every release, newest first, which one is live, who published it and
// when. Those who may publish the app can roll back to an earlier release, which points the app at
// it and changes no data; the server refuses a release the app's data doesn't fit and says which
// fields, and checks every change again. Shown to those who may design or publish the app.

import { useState } from 'react'
import { ApiError, type App, type AppRelease, api, errorText } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'

export function AppReleases({
  base,
  app,
  canRollBack,
  changed,
}: {
  // …/spaces/{space}/apps/{app}
  base: string
  app: App
  canRollBack: boolean
  // Called when the app changed (a rollback, or a conflict): the caller reloads the app.
  changed: () => void
}) {
  const t = useT()
  const f = useFormat()
  // The history is read again whenever the app's version moves: a publish elsewhere adds a release.
  const loaded = useLoad(
    () => api<{ releases: AppRelease[] }>(`${base}/releases`).then((r) => r.releases),
    [base, app.version],
  )
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const rollBack = async (release: AppRelease) => {
    setBusy(true)
    try {
      await api<App>(`${base}/rollback`, { method: 'POST', body: { version: app.version, release: release.number } })
      setError(null)
    } catch (e) {
      setError(
        e instanceof ApiError && e.status === 409 && e.code === 'app_changed'
          ? t('apps.changed', { name: app.name })
          : errorText(e),
      )
    }
    setBusy(false)
    changed()
  }

  return (
    <div className="tt-app-releases">
      <h3>{t('apps.releases.title')}</h3>
      {(loaded.error || error) && (
        <p role="alert" className="tt-error">
          {error ?? loaded.error}
        </p>
      )}
      {!loaded.data && !loaded.error && <p className="tt-muted">{t('app.loading')}</p>}
      {loaded.data && loaded.data.length === 0 && <p className="tt-muted">{t('apps.releases.none')}</p>}
      {loaded.data && loaded.data.length > 0 && (
        <ul className="tt-release-list">
          {loaded.data.map((r) => (
            <li key={r.number} className="tt-release" data-live={r.live || undefined}>
              <span className="tt-release-name">
                <strong>{t('apps.releases.number', { number: r.number })}</strong>
                {r.live && (
                  <span className="tt-stage-label" data-stage="live">
                    {t('apps.releases.live')}
                  </span>
                )}
              </span>
              <span className="tt-muted tt-small">
                {r.published_by
                  ? t('apps.releases.publishedBy', { name: r.published_by.name, date: f.dateTime(r.published_at) })
                  : t('apps.releases.published', { date: f.dateTime(r.published_at) })}
              </span>
              {canRollBack && !r.live && (
                <button
                  type="button"
                  className="tt-button"
                  disabled={busy}
                  onClick={() => rollBack(r)}
                  aria-label={t('apps.releases.rollBackNamed', { number: r.number })}
                >
                  {t('apps.releases.rollBack')}
                </button>
              )}
            </li>
          ))}
        </ul>
      )}
      {canRollBack && <p className="tt-muted tt-small">{t('apps.releases.rollBackNote')}</p>}
    </div>
  )
}
