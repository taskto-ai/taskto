import { lazy, type ReactNode, Suspense, useEffect, useRef, useState } from 'react'
import type { Me, Workspace } from '../api'
import { parseRunPath } from '../apps/route'
import { Desk } from '../desk/Desk'
import { setPreferences, useT } from '../i18n'
import { useMediaQuery } from '../media'
import { EMAIL_CONFIRM_PATH, parseNotificationPath } from '../notifications/api'
import { ConfirmEmail } from '../notifications/ConfirmEmail'
import { OpenNotification } from '../notifications/NotificationsPage'
import { usePushSync } from '../notifications/usePushSync'
import { navigate, parseRoute, type Route, routePath, usePath } from '../router'
import { Button } from '../ui/Button'
import { modalOpen } from '../ui/layers'
import { Notice } from '../ui/Notice'
import { Page as PageFrame } from '../ui/Page'
import { Toaster } from '../ui/Toast'
import { ErrorBoundary } from './ErrorBoundary'
import { NoWorkspace } from './NoWorkspace'
import { MAIN_ID } from './nav'
import { PhoneBar } from './PhoneBar'
import { Loading, SectionPage } from './SectionPage'
import { Sidebar } from './Sidebar'
import './Shell.css'

const StandaloneApp = lazy(() => import('../apps/StandaloneApp').then((m) => ({ default: m.StandaloneApp })))

const LAST_WORKSPACE = 'taskto.workspace'

function lastWorkspace(): number | null {
  try {
    return Number(localStorage.getItem(LAST_WORKSPACE)) || null
  } catch {
    return null
  }
}

function focusPane(main: HTMLElement | null) {
  const pane = main?.querySelector<HTMLElement>('[data-place][data-focused="true"]')
  ;(pane ?? main)?.focus({ preventScroll: true })
}

function homeWorkspace(me: Me): Workspace | undefined {
  return me.workspaces.find((w) => w.id === lastWorkspace()) ?? me.workspaces[0]
}

function rememberWorkspace(id: number) {
  try {
    localStorage.setItem(LAST_WORKSPACE, String(id))
  } catch {}
}

export function Shell({ me, onLogout, onMe }: { me: Me; onLogout: () => void; onMe?: (me: Me) => void }) {
  return (
    <>
      <Toaster />
      <ShellRoute me={me} onLogout={onLogout} onMe={onMe} />
    </>
  )
}

function ShellRoute({ me, onLogout, onMe }: { me: Me; onLogout: () => void; onMe?: (me: Me) => void }) {
  const path = usePath()
  const opening = parseNotificationPath(path)
  const first = homeWorkspace(me)?.id ?? null
  const running = parseRunPath(path)
  if (running) {
    return (
      <ErrorBoundary fallback={<PageFailed />} resetKey={path}>
        <Suspense fallback={<Loading />}>
          <StandaloneApp
            key={path}
            me={me}
            workspace={running.workspace}
            space={running.space}
            app={running.app}
            screen={running.screen}
          />
        </Suspense>
      </ErrorBoundary>
    )
  }
  if (path === EMAIL_CONFIRM_PATH) {
    const page = <ConfirmEmail token={window.location.hash.slice(1)} workspace={first} />
    return <Frame me={me} onLogout={onLogout} onMe={onMe} special={{ section: 'settings', workspace: first, page }} />
  }
  if (opening !== null) {
    const page = <OpenNotification id={opening} fallbackWorkspace={first} />
    return (
      <Frame me={me} onLogout={onLogout} onMe={onMe} special={{ section: 'notifications', workspace: first, page }} />
    )
  }
  return <Frame me={me} onLogout={onLogout} onMe={onMe} />
}

interface SpecialPage {
  section: Route['section']
  workspace: number | null
  page: ReactNode
}

function Frame({
  me,
  onLogout,
  onMe,
  special,
}: {
  me: Me
  onLogout: () => void
  onMe?: (me: Me) => void
  special?: SpecialPage
}) {
  const path = usePath()
  usePushSync(me.user.id, me.workspaces.length > 0)
  const parsed = parseRoute(path)
  const member = (id: number) => me.workspaces.some((w) => w.id === id)
  const fallback = homeWorkspace(me)
  const route: Route | null = special
    ? special.workspace !== null
      ? { workspace: special.workspace, section: special.section }
      : null
    : parsed && member(parsed.workspace)
      ? parsed
      : fallback
        ? { workspace: fallback.id, section: parsed?.section ?? 'conversations' }
        : null
  const [drawer, setDrawer] = useState(false)
  const drawerOpen = useRef(false)
  drawerOpen.current = drawer
  const menuButton = useRef<HTMLButtonElement>(null)
  const sidebar = useRef<HTMLDivElement>(null)
  const main = useRef<HTMLElement>(null)
  const t = useT()
  const wide = useMediaQuery('(min-width: 56.2501rem)')

  const target = route && !special ? routePath(route) : null
  useEffect(() => {
    if (target && target !== path) navigate(target, { replace: true })
  }, [target, path])
  const shown = route?.workspace
  useEffect(() => {
    if (shown) rememberWorkspace(shown)
  }, [shown])
  const workspaceLocale = me.workspaces.find((w) => w.id === shown)?.locale ?? null
  useEffect(() => {
    void setPreferences({ workspace: workspaceLocale })
  }, [workspaceLocale])
  const focusPage = useRef(false)
  const followedFrom = useRef<string | null>(null)
  const shownPage = special ? path : target
  const drawnAt = useRef(shownPage)
  useEffect(() => {
    if (drawnAt.current === shownPage) return
    drawnAt.current = shownPage
    if (drawerOpen.current) focusPage.current = true
    setDrawer(false)
  }, [shownPage])
  useEffect(() => {
    if (drawer || !focusPage.current) return
    focusPane(main.current)
    if (followedFrom.current !== null && followedFrom.current === shownPage) return
    focusPage.current = false
    followedFrom.current = null
  }, [drawer, shownPage])
  useEffect(() => {
    if (!drawer) return
    focusPage.current = false
    followedFrom.current = null
    ;(
      sidebar.current?.querySelector<HTMLElement>('[aria-current="page"]') ??
      sidebar.current?.querySelector<HTMLElement>('a')
    )?.focus()
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented || modalOpen()) return
      setDrawer(false)
      menuButton.current?.focus()
    }
    const onClick = (e: MouseEvent) => {
      if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
      const link = e.target instanceof Element ? e.target.closest('a[href]') : null
      if (!link || !sidebar.current?.contains(link)) return
      focusPage.current = true
      followedFrom.current = drawnAt.current
      setDrawer(false)
    }
    document.addEventListener('keydown', onKey)
    window.addEventListener('click', onClick, true)
    return () => {
      document.removeEventListener('keydown', onKey)
      window.removeEventListener('click', onClick, true)
    }
  }, [drawer])

  if (!route) return special ? special.page : <NoWorkspace me={me} onLogout={onLogout} onMe={onMe} />

  const workspace = me.workspaces.find((w) => w.id === route.workspace) ?? me.workspaces[0]
  const desk = wide && !special
  return (
    <div className="ui-shell" data-drawer={drawer ? 'open' : 'closed'}>
      <Button className="ui-shell-skip" onClick={() => focusPane(main.current)}>
        {t('shell.skipToContent')}
      </Button>
      <PhoneBar drawer={drawer} onToggle={() => setDrawer((d) => !d)} menuButton={menuButton} t={t} />
      <Sidebar
        me={me}
        workspace={workspace}
        route={route}
        path={path}
        onLogout={onLogout}
        onMe={onMe}
        sidebarRef={sidebar}
        t={t}
      />
      {drawer && <div className="ui-shell-scrim" aria-hidden="true" onClick={() => setDrawer(false)} />}
      <main
        id={MAIN_ID}
        ref={main}
        tabIndex={-1}
        className={desk ? 'ui-shell-main desk-main' : 'ui-shell-main'}
        inert={drawer || undefined}
        key={special ? path : desk ? `desk/${workspace.id}` : `${workspace.id}/${route.section}`}
      >
        <ErrorBoundary fallback={<PageFailed />} resetKey={path}>
          {special ? (
            special.page
          ) : desk && target ? (
            <Desk me={me} workspace={workspace} path={target} failed={<PageFailed />} />
          ) : (
            <SectionPage route={route} me={me} workspace={workspace} />
          )}
        </ErrorBoundary>
      </main>
    </div>
  )
}

function PageFailed() {
  const t = useT()
  return (
    <PageFrame>
      <Notice tone="danger" action={<Button onClick={() => window.location.reload()}>{t('shell.reload')}</Button>}>
        {t('shell.pageFailed')}
      </Notice>
    </PageFrame>
  )
}
