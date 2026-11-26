// The signed-in layout, led by the sidebar (Sidebar.tsx: the logo, the workspace switcher, the
// sections, the inbox, the pinned apps, Settings and the account) beside the page, which owns its
// title. There is no top bar on a wide screen; on a tablet the sidebar is a rail of icons, and on a
// phone a slim bar (PhoneBar.tsx) has the menu button that opens the sidebar as a drawer. The URL
// names the workspace and section (/w/{workspace}/{section}); a URL that names neither, or a
// workspace the user isn't in, goes to the last workspace used.

import { lazy, type ReactNode, Suspense, useEffect, useRef, useState } from 'react'
import type { Me, Workspace } from '../api'
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

// The A2UI renderer is most of the app's code: loaded when Apps is opened.
const AppsPage = lazy(() => import('../apps/AppsPage').then((m) => ({ default: m.AppsPage })))
// Home draws its widgets with it too.
const HomePage = lazy(() => import('../home/HomePage').then((m) => ({ default: m.HomePage })))

const LAST_WORKSPACE = 'taskto.workspace'

function lastWorkspace(): number | null {
  try {
    return Number(localStorage.getItem(LAST_WORKSPACE)) || null
  } catch {
    return null
  }
}

// The workspace to show when the address names none of the person's: the one used last, else the
// first.
function homeWorkspace(me: Me): Workspace | undefined {
  return me.workspaces.find((w) => w.id === lastWorkspace()) ?? me.workspaces[0]
}

function rememberWorkspace(id: number) {
  try {
    localStorage.setItem(LAST_WORKSPACE, String(id))
  } catch {
    // Not remembered: the first workspace next time.
  }
}

// Two addresses that come from outside the app are pages of the frame without being sections: a
// push's /notification/{id} (read as the signed-in person and followed to where it leads) and an
// email's confirmation link. They are drawn inside the frame (the sidebar, the skip link, <main>),
// with the section they belong to marked, and the frame doesn't take their address for an unknown
// route and rewrite it. Someone signed out never gets here: the sign-in screen comes first.
// `onMe` takes the account's new state when the shell changed it (a workspace was created).
//
// The toasts' stack is drawn here, once, before anything shows one: its live regions are then on
// the page, empty, when the first toast comes, so that one is said too. Only the shell's screens
// show toasts (the account menu, "New workspace"), the one for someone with no workspace included.
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
  // The workspace the frame shows (the one used last, else the first): the page's own links (to
  // the inbox, to the notification settings) go to it too.
  const first = homeWorkspace(me)?.id ?? null
  if (path === EMAIL_CONFIRM_PATH) {
    const page = <ConfirmEmail token={window.location.hash.slice(1)} workspace={first} />
    return <Frame me={me} onLogout={onLogout} onMe={onMe} special={{ section: 'settings', page }} />
  }
  if (opening !== null) {
    const page = <OpenNotification id={opening} fallbackWorkspace={first} />
    return <Frame me={me} onLogout={onLogout} onMe={onMe} special={{ section: 'notifications', page }} />
  }
  return <Frame me={me} onLogout={onLogout} onMe={onMe} />
}

// A page of the frame that is not a section's (above): what it draws, and the section it belongs to.
interface SpecialPage {
  section: Route['section']
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
    ? fallback
      ? { workspace: fallback.id, section: special.section }
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

  // A special page keeps its own address (its link's): the frame rewrites only a section's.
  const target = route && !special ? routePath(route) : null
  useEffect(() => {
    if (target && target !== path) navigate(target, { replace: true })
  }, [target, path])
  const shown = route?.workspace
  useEffect(() => {
    if (shown) rememberWorkspace(shown)
  }, [shown])
  // The workspace's default language, for a browser that asks for none the app has.
  const workspaceLocale = me.workspaces.find((w) => w.id === shown)?.locale ?? null
  useEffect(() => {
    void setPreferences({ workspace: workspaceLocale })
  }, [workspaceLocale])
  // A page picked in the drawer closes it, and focus goes to the page, since the link that
  // had it is hidden with the drawer. The page is inert while the drawer is open, and an inert
  // element can't take focus: it is focused once the drawer has closed (the effect below).
  const focusPage = useRef(false)
  // Only a change of page closes it, not the first draw: that effect runs after the draw, and a
  // click on the menu button before it would be undone. The page is the resolved route, not the
  // address: the shell rewriting an address to its canonical form (a trailing slash, a default
  // page, another workspace) isn't a navigation. A hash or a query never reaches either.
  const drawnAt = useRef(target)
  useEffect(() => {
    if (drawnAt.current === target) return
    drawnAt.current = target
    if (drawerOpen.current) focusPage.current = true
    setDrawer(false)
  }, [target])
  useEffect(() => {
    if (drawer || !focusPage.current) return
    focusPage.current = false
    main.current?.focus({ preventScroll: true })
  }, [drawer])
  // An open drawer takes focus, Escape closes it and gives focus back to its button.
  useEffect(() => {
    if (!drawer) return
    ;(
      sidebar.current?.querySelector<HTMLElement>('[aria-current="page"]') ??
      sidebar.current?.querySelector<HTMLElement>('a')
    )?.focus()
    const onKey = (e: KeyboardEvent) => {
      // An Escape a menu has taken is the menu's, and one in a dialog (a new workspace's) is the
      // dialog's: the drawer stays.
      if (e.key !== 'Escape' || e.defaultPrevented || modalOpen()) return
      setDrawer(false)
      menuButton.current?.focus()
    }
    // A link of the drawer that is followed closes it at once, before the page hears of the click:
    // a page that asks before it is left (a draft) shows its question in the page, which is inert
    // while the drawer is open. On the window, in the capture phase, so it comes before a handler
    // on the document that stops the click. A click that opens a new tab leaves it open.
    const onClick = (e: MouseEvent) => {
      if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
      const link = e.target instanceof Element ? e.target.closest('a[href]') : null
      if (!link || !sidebar.current?.contains(link)) return
      focusPage.current = true
      setDrawer(false)
    }
    document.addEventListener('keydown', onKey)
    window.addEventListener('click', onClick, true)
    return () => {
      document.removeEventListener('keydown', onKey)
      window.removeEventListener('click', onClick, true)
    }
  }, [drawer])

  // With no workspace there is no frame to draw a special page in: it is drawn alone, as before.
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
      {drawer && (
        // A press on the page beside the open drawer closes it. Only for a pointer: the keyboard
        // has Escape and the menu button, and a screen reader the menu button.
        <div className="ui-shell-scrim" aria-hidden="true" onClick={() => setDrawer(false)} />
      )}
      <main
        id={MAIN_ID}
        ref={main}
        tabIndex={-1}
        className="ui-shell-main"
        inert={drawer || undefined}
        key={special ? path : `${workspace.id}/${route.section}`}
      >
        {/* A page that fails to draw leaves the shell around it (the menu, the settings) usable. */}
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
        <Suspense fallback={<Loading />}>
          <AppsPage workspace={route.workspace} open={route.item} />
        </Suspense>
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

// A page whose code is still on its way: the shape of a page, and the word for a screen reader.
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
