import { type ReactNode, useEffect, useRef, useState } from 'react'
import { ACCESS_LOST, ApiError, accessLostConcerns, api, type Me, type Space, ws } from '../api'
import { setPreferences, useT } from '../i18n'
import { useLoad } from '../load'
import { routePath } from '../router'
import { Button, ButtonLink } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { Notice } from '../ui/Notice'
import { Page } from '../ui/Page'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { WorkspaceAdmin } from './admin'
import { LaunchedApp } from './LaunchedApp'
import './LaunchedApp.css'

export function StandaloneApp({
  me,
  workspace,
  space,
  app,
  screen,
}: {
  me: Me
  workspace: number
  space: number
  app: number
  screen?: string
}) {
  const t = useT()
  const main = useRef<HTMLElement>(null)
  const member = me.workspaces.find((w) => w.id === workspace)
  const outdated = useRef(false)
  const spaces = useLoad(
    () =>
      member
        ? api<{ spaces: Space[] }>(`${ws(workspace)}/spaces`).then(
            (r) => r.spaces,
            (e: unknown) => {
              if (e instanceof ApiError && e.status === 404) {
                outdated.current = false
                return []
              }
              throw e
            },
          )
        : Promise.resolve([]),
    [workspace, !!member],
  )
  const reloading = useRef(false)
  reloading.current = !!spaces.loading
  const reload = spaces.reload
  const [denied, setDenied] = useState(false)
  useEffect(() => {
    if (spaces.loading) return
    if (outdated.current) {
      outdated.current = false
      reload()
    } else if (!spaces.error) setDenied(false)
  }, [spaces.loading, spaces.error, reload])
  useEffect(() => {
    const lost = (e: Event) => {
      if (!accessLostConcerns(e, workspace, space)) return
      setDenied(true)
      if (reloading.current) outdated.current = true
      else reload()
    }
    const wake = () => {
      if (document.visibilityState === 'visible' && !reloading.current) reload()
    }
    window.addEventListener(ACCESS_LOST, lost)
    window.addEventListener('focus', wake)
    document.addEventListener('visibilitychange', wake)
    return () => {
      window.removeEventListener(ACCESS_LOST, lost)
      window.removeEventListener('focus', wake)
      document.removeEventListener('visibilitychange', wake)
    }
  }, [reload, workspace, space])
  const locale = member?.locale ?? null
  useEffect(() => {
    void setPreferences({ workspace: locale })
  }, [locale])
  const found = denied ? undefined : spaces.data?.find((s) => s.id === space)

  let body: ReactNode
  if (!member || (spaces.data && !found) || (denied && spaces.error)) {
    body = (
      <Page width="reading">
        <EmptyState
          title={t('apps.standaloneNotFound')}
          actions={
            <ButtonLink href={member ? routePath({ workspace, section: 'apps' }) : '/'}>
              {t(member ? 'apps.all' : 'shell.sections.home')}
            </ButtonLink>
          }
        >
          <p>{t('apps.unavailable')}</p>
        </EmptyState>
      </Page>
    )
  } else if (!found && spaces.error) {
    body = (
      <Page width="reading">
        <Notice tone="danger">{spaces.error}</Notice>
      </Page>
    )
  } else if (!found) {
    body = (
      <Page aria-busy="true">
        <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
        <Skeleton shape="block" height="calc(var(--space-16) * 4)" />
      </Page>
    )
  } else {
    body = (
      <LaunchedApp
        standalone
        workspace={workspace}
        space={space}
        app={app}
        writable={found.access === 'write'}
        screen={screen}
        spaceInfo={found}
      />
    )
  }

  return (
    <div className="ui-standalone">
      <Button className="ui-shell-skip" onClick={() => main.current?.focus()}>
        {t('shell.skipToContent')}
      </Button>
      <main ref={main} tabIndex={-1} className="ui-standalone-main">
        <WorkspaceAdmin value={member?.role === 'admin'}>{body}</WorkspaceAdmin>
      </main>
    </div>
  )
}
