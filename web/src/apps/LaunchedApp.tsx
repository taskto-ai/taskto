// One launched app on the Apps page: its header (its name, the way back to every app, to its records
// and to editing it with the agent), what the caller may do with it, the app itself (AppStage), what
// its buttons set going (AppEffects), its records (AppRecords) and, quietly at the end, its use
// offline (OfflinePanel). An app that is offline says so, and those who may publish it restore it.

import { useId, useState } from 'react'
import { type App, api, type Space, ws } from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { OfflinePanel } from '../offline/OfflinePanel'
import { Link, routePath } from '../router'
import { Button, ButtonLink } from '../ui/Button'
import { Notice } from '../ui/Notice'
import { Page } from '../ui/Page'
import { PageHeader } from '../ui/PageHeader'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { AppEffects } from './AppEffects'
import { AppRecords, RecordsButton } from './AppRecords'
import { AppStage } from './AppStage'
import { builderPath } from './route'
import './LaunchedApp.css'

export function LaunchedApp({
  workspace,
  space,
  app,
  writable,
  screen,
  spaceInfo,
}: {
  workspace: number
  space: number
  app: number
  writable: boolean
  screen?: string
  spaceInfo?: Space
}) {
  const base = `${ws(workspace)}/spaces/${space}/apps/${app}`
  const info = useLoad(() => api<App>(base), [base])
  // Bumped when a record is restored, so the screen is loaded again with it.
  const [restored, setRestored] = useState(0)
  // How many actions ran on the screen: each may have set effects going.
  const [actions, setActions] = useState(0)
  const [recordsOpen, setRecordsOpen] = useState(false)
  const recordsId = useId()
  const back = routePath({ workspace, section: 'apps' })
  const t = useT()
  const data = info.data
  const restore = async () => {
    if (!data) return
    try {
      await api<App>(base, { method: 'PATCH', body: { version: data.version, archived: false } })
    } finally {
      info.reload()
    }
  }
  const running = data && !data.archived_at

  // What the caller may do with it, said once under its name.
  const notes = running
    ? [
        !writable && t('apps.viewOnly'),
        writable && !data.access.submit && data.access.modify_records === 'none' && t('apps.noDataChanges'),
        data.access.read_records === 'own' && t('apps.ownRecords'),
      ].filter((n): n is string => !!n)
    : []

  return (
    <Page width="list">
      <PageHeader
        path={
          <Link className="ui-launched-back" href={back}>
            {t('apps.all')}
          </Link>
        }
        title={data?.name ?? t('apps.app')}
        help={notes.length > 0 ? notes.join(' ') : undefined}
        actions={
          running && (
            <>
              <RecordsButton
                access={data.access}
                open={recordsOpen}
                controls={recordsId}
                onToggle={() => setRecordsOpen((o) => !o)}
              />
              {writable && data.access.design && (
                <ButtonLink
                  icon="edit"
                  href={builderPath(workspace, { kind: 'edit', space, app })}
                  aria-label={t('apps.builder.editNamed', { name: data.name })}
                >
                  {t('apps.builder.edit')}
                </ButtonLink>
              )}
            </>
          )
        }
      />
      {info.error && <Notice tone="danger">{info.error}</Notice>}
      {!data && !info.error && (
        <div className="ui-launched-loading" role="status">
          <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
          <Skeleton shape="block" height="calc(var(--space-16) * 4)" />
        </div>
      )}
      {data?.archived_at && (
        <Notice
          tone="neutral"
          action={
            writable &&
            data.access.publish && (
              <Button variant="primary" onClick={restore}>
                {t('apps.restore')}
              </Button>
            )
          }
        >
          {t('apps.offlineNotice')}
        </Notice>
      )}
      {running && (
        <>
          <AppStage
            key={screen ?? ''}
            base={base}
            app={data}
            reloadKey={restored}
            screen={screen}
            onAction={() => setActions((n) => n + 1)}
          />
          <AppEffects workspace={workspace} base={base} writable={writable} reloadKey={actions} />
          <AppRecords
            id={recordsId}
            open={recordsOpen}
            onClose={() => setRecordsOpen(false)}
            base={base}
            appId={app}
            access={data.access}
            writable={writable}
            definition={data.live?.definition}
            release={data.live?.number}
            onRestored={() => setRestored((n) => n + 1)}
          />
          <OfflinePanel workspace={workspace} space={space} app={data} spaceInfo={spaceInfo} />
        </>
      )}
    </Page>
  )
}
