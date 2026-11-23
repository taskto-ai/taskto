// An app the agent saved, shown in the conversation where it did: launched the same way the Apps
// page launches it (AppStage), with a link to open it there. What it shows is labelled: the live
// release, which people use and whose actions run as they do there (same session, CSRF guard and
// the server's check that the user may write the space), or its draft, a preview, when the agent's
// changes aren't published yet and the person may design or publish the app. An app the user
// can't launch (gone, moved, in a space they can no longer read, or offline) is a short note
// instead.

import { ApiError, type App, api, ws } from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { Link } from '../router'
import { AppStage } from './AppStage'
import { appPath } from './route'

export function AppPreview({ workspace, space, app }: { workspace: number; space: number; app: number }) {
  const base = `${ws(workspace)}/spaces/${space}/apps/${app}`
  // null: the server has no such app for this user, the same answer for one in a space they
  // can't read.
  const info = useLoad(
    () =>
      api<App>(base).catch((e: unknown) => {
        if (e instanceof ApiError && e.status === 404) return null
        throw e
      }),
    [base],
  )
  const t = useT()

  if (info.data === null) {
    return <p className="tt-app-preview tt-muted tt-small">{t('apps.unavailable')}</p>
  }
  return (
    <div className="tt-app-preview">
      {info.error ? (
        <p className="tt-muted tt-small">{t('apps.cantShow', { error: info.error })}</p>
      ) : info.data?.archived_at ? (
        <p className="tt-muted tt-small">{t('apps.offlineNamed', { name: info.data.name })}</p>
      ) : info.data ? (
        <AppStage base={base} app={info.data} />
      ) : (
        <p className="tt-muted tt-small">{t('app.loading')}</p>
      )}
      <Link
        className="tt-small"
        href={appPath(workspace, space, app)}
        aria-label={info.data ? t('apps.openNamedInApps', { name: info.data.name }) : undefined}
      >
        {t('apps.openInApps')}
      </Link>
    </div>
  )
}
