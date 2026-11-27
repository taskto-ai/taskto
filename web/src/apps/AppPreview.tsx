import { ApiError, type App, api, ws } from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { Link } from '../router'
import { Icon } from '../ui/Icon'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { AppStage } from './AppStage'
import { appPath } from './route'
import './AppPreview.css'

export function AppPreview({ workspace, space, app }: { workspace: number; space: number; app: number }) {
  const base = `${ws(workspace)}/spaces/${space}/apps/${app}`
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
    return <p className="ui-app-preview ui-app-preview-note">{t('apps.unavailable')}</p>
  }
  return (
    <div className="ui-app-preview">
      {info.error ? (
        <p className="ui-app-preview-note">{t('apps.cantShow', { error: info.error })}</p>
      ) : info.data?.archived_at ? (
        <p className="ui-app-preview-note">{t('apps.offlineNamed', { name: info.data.name })}</p>
      ) : info.data ? (
        <AppStage base={base} app={info.data} />
      ) : (
        <div className="ui-app-preview-loading" role="status">
          <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
          <Skeleton shape="block" height="calc(var(--space-16) * 2)" />
        </div>
      )}
      <Link
        className="ui-app-preview-open"
        href={appPath(workspace, space, app)}
        aria-label={info.data ? t('apps.openNamedInApps', { name: info.data.name }) : undefined}
      >
        {t('apps.openInApps')}
        <Icon name="chevron-end" size={14} />
      </Link>
    </div>
  )
}
