import { useEffect, useId, useRef, useState } from 'react'
import { type App, api, type Space, ws } from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { OfflinePanel } from '../offline/OfflinePanel'
import { BRAND } from '../shell/nav'
import { Button, ButtonLink } from '../ui/Button'
import { Notice } from '../ui/Notice'
import { Page } from '../ui/Page'
import { PageHeader } from '../ui/PageHeader'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { AppEffects } from './AppEffects'
import { AppRecords, RecordsButton } from './AppRecords'
import { AppStage } from './AppStage'
import { BackToApps } from './AppTemplates'
import { appPath, builderPath, openRunWindow, runPath } from './route'
import './LaunchedApp.css'

export function LaunchedApp({
  workspace,
  space,
  app,
  writable,
  screen,
  spaceInfo,
  standalone = false,
}: {
  workspace: number
  space: number
  app: number
  writable: boolean
  screen?: string
  spaceInfo?: Space
  standalone?: boolean
}) {
  const base = `${ws(workspace)}/spaces/${space}/apps/${app}`
  const info = useLoad(() => api<App>(base), [base])
  const [restored, setRestored] = useState(0)
  const [actions, setActions] = useState(0)
  const [recordsOpen, setRecordsOpen] = useState(false)
  const recordsId = useId()
  const recordsToggle = useRef<HTMLButtonElement>(null)
  const leaveRecords = useRef<((go: () => void) => void) | null>(null)
  const closeRecords = (fromInside: boolean) => {
    const go = () => {
      setRecordsOpen(false)
      if (fromInside) recordsToggle.current?.focus()
    }
    if (leaveRecords.current) leaveRecords.current(go)
    else go()
  }
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
  const name = data?.name
  useEffect(() => {
    if (!standalone || !name) return
    const before = document.title
    document.title = `${name} · ${BRAND}`
    return () => {
      document.title = before
    }
  }, [standalone, name])

  const notes = running
    ? [
        !writable && t('apps.viewOnly'),
        writable && !data.access.submit && data.access.modify_records === 'none' && t('apps.noDataChanges'),
        data.access.read_records === 'own' && t('apps.ownRecords'),
      ].filter((n): n is string => !!n)
    : []

  return (
    <Page width={standalone ? 'full' : 'list'}>
      <PageHeader
        path={standalone ? undefined : <BackToApps workspace={workspace} />}
        title={data?.name ?? t('apps.app')}
        help={notes.length > 0 ? notes.join(' ') : undefined}
        actions={
          running && (
            <>
              <RecordsButton
                access={data.access}
                open={recordsOpen}
                controls={recordsId}
                ref={recordsToggle}
                onToggle={() => (recordsOpen ? closeRecords(false) : setRecordsOpen(true))}
              />
              {standalone ? (
                <ButtonLink variant="ghost" icon="apps" href={appPath(workspace, space, app, screen)}>
                  {t('apps.openInTaskto')}
                </ButtonLink>
              ) : (
                <>
                  <Button variant="ghost" icon="browser" onClick={() => openRunWindow(workspace, space, app, screen)}>
                    {t('apps.openWindow')}
                  </Button>
                  <ButtonLink variant="ghost" href={runPath(workspace, space, app, screen)} target="_blank">
                    {t('apps.openTab')}
                  </ButtonLink>
                </>
              )}
              {!standalone && writable && data.access.design && (
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
            onClose={() => closeRecords(true)}
            leaveRef={leaveRecords}
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
