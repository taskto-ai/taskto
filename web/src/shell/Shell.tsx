import { lazy, type ReactNode, Suspense, useEffect, useRef, useState } from 'react'
import type { Me, Workspace } from '../api'
import { WorkspaceAdmin } from '../apps/admin'
import { parseRunPath } from '../apps/route'
import { BoardsPage } from '../boards/BoardsPage'
import { ConversationsPage } from '../conversations/ConversationsPage'
import { setPreferences, useT } from '../i18n'
import { KnowledgePage } from '../knowledge/KnowledgePage'
import { EMAIL_CONFIRM_PATH, parseNotificationPath } from '../notifications/api'
import { ConfirmEmail } from '../notifications/ConfirmEmail'
import { NotificationsPage, OpenNotification } from '../notifications/NotificationsPage'
import { usePushSync } from '../notifications/usePushSync'
import { navigate, parseRoute, type Route, routePath, usePath } from '../router'
import { SettingsPage } from '../settings/SettingsPage'
import { Button } from '../ui/Button'
import { modalOpen } from '../ui/layers'
import { Notice } from '../ui/Notice'
import { Page as PageFrame } from '../ui/Page'
import { Skeleton } from '../ui/Skeleton'
import { Toaster } from '../ui/Toast'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { ErrorBoundary } from './ErrorBoundary'
import { NoWorkspace } from './NoWorkspace'
import { MAIN_ID } from './nav'
import { PhoneBar } from './PhoneBar'
import { Sidebar } from './Sidebar'
import './Shell.css'

const AppsPage = lazy(() => import('../apps/AppsPage').then((m) => ({ default: m.AppsPage })))
const StandaloneApp = lazy(() => import('../apps/StandaloneApp').then((m) => ({ default: m.StandaloneApp })))
const HomePage = lazy(() => import('../home/HomePage').then((m) => ({ default: m.HomePage })))

const LAST_WORKSPACE = 'taskto.workspace'

function lastWorkspace(): number | null {
  try {
    return Number(localStorage.getItem(LAST_WORKSPACE)) || null
  } catch {
    return null
  }
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
    main.current?.focus({ preventScroll: true })
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
  return (
    <div className="ui-shell" data-drawer={drawer ? 'open' : 'closed'}>
      <Button className="ui-shell-skip" onClick={() => main.current?.focus()}>
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
        className="ui-shell-main"
        inert={drawer || undefined}
        key={special ? path : `${workspace.id}/${route.section}`}
      >
        <ErrorBoundary fallback={<PageFailed />} resetKey={path}>
          {special ? special.page : <Page route={route} me={me} workspace={workspace} />}
        </ErrorBoundary>
      </main>
    </div>
  )
}

function Page({ route, me, workspace }: { route: Route; me: Me; workspace: Workspace }): ReactNode {
  switch (route.section) {
    case 'home':
      return (
        <Suspense fallback={<Loading />}>
          <HomePage workspace={route.workspace} admin={workspace.role === 'admin'} />
        </Suspense>
      )
    case 'conversations':
      return <ConversationsPage workspace={route.workspace} open={route.item} />
    case 'knowledge':
      return (
        <KnowledgePage
          key={route.item ?? ''}
          workspace={route.workspace}
          admin={workspace.role === 'admin'}
          open={route.item}
        />
      )
    case 'boards':
      return <BoardsPage key={route.item ?? ''} workspace={route.workspace} open={route.item} />
    case 'apps':
      return (
        <WorkspaceAdmin value={workspace.role === 'admin'}>
          <Suspense fallback={<Loading />}>
            <AppsPage workspace={route.workspace} open={route.item} />
          </Suspense>
        </WorkspaceAdmin>
      )
    case 'settings':
      return <SettingsPage me={me} workspace={workspace} tab={route.item} />
    case 'notifications':
      return <NotificationsPage workspace={route.workspace} />
  }
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

function Loading() {
  const t = useT()
  return (
    <PageFrame aria-busy="true">
      <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
      <Skeleton width="40%" />
      <Skeleton shape="block" height="160px" />
      <Skeleton width="70%" />
    </PageFrame>
  )
}
