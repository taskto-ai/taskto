// The signed-in layout: a top bar (workspace, theme, account), the sidebar with the
// workspace's sections, and the page. On a phone the sidebar is a drawer behind the menu
// button. The URL names the workspace and section (/w/{workspace}/{section}); a URL that
// names neither, or a workspace the user isn't in, goes to the last workspace used.

import { lazy, type ReactNode, Suspense, useEffect, useRef, useState } from 'react'
import { api, errorText, type Me, type Workspace } from '../api'
import { BrandLogo } from '../BrandLogo'
import { BoardsPage } from '../boards/BoardsPage'
import { ConversationsPage } from '../conversations/ConversationsPage'
import { PinnedNav } from '../home/PinnedNav'
import {
  automaticLocale,
  type Key,
  LOCALES,
  localeName,
  setPreferences,
  type T,
  useFormat,
  useLocale,
  useT,
} from '../i18n'
import { KnowledgePage } from '../knowledge/KnowledgePage'
import { EMAIL_CONFIRM_PATH, parseNotificationPath } from '../notifications/api'
import { ConfirmEmail } from '../notifications/ConfirmEmail'
import { NotificationsPage, OpenNotification } from '../notifications/NotificationsPage'
import { usePushSync } from '../notifications/usePushSync'
import { useUnread } from '../notifications/useUnread'
import { useInstall } from '../pwa/install'
import { Link, navigate, parseRoute, type Route, routePath, type Section, usePath } from '../router'
import { SettingsPage } from '../settings/SettingsPage'
import { type Theme, useTheme } from '../theme'
import { ErrorBoundary } from './ErrorBoundary'
import { Icon } from './Icon'
import { NewWorkspace } from './NewWorkspace'

// The A2UI renderer is most of the app's code: loaded when Apps is opened.
const AppsPage = lazy(() => import('../apps/AppsPage').then((m) => ({ default: m.AppsPage })))
// Home draws its widgets with it too.
const HomePage = lazy(() => import('../home/HomePage').then((m) => ({ default: m.HomePage })))

const LAST_WORKSPACE = 'taskto.workspace'

const NAV: { section: Exclude<Section, 'notifications'>; label: Key }[] = [
  { section: 'home', label: 'shell.sections.home' },
  { section: 'conversations', label: 'shell.sections.conversations' },
  { section: 'knowledge', label: 'shell.sections.knowledge' },
  { section: 'boards', label: 'shell.sections.boards' },
  { section: 'apps', label: 'shell.sections.apps' },
  { section: 'settings', label: 'shell.sections.settings' },
]

// The product's name, the same in every language.
const BRAND = 'TaskTo.AI'

function lastWorkspace(): number | null {
  try {
    return Number(localStorage.getItem(LAST_WORKSPACE)) || null
  } catch {
    return null
  }
}

function rememberWorkspace(id: number) {
  try {
    localStorage.setItem(LAST_WORKSPACE, String(id))
  } catch {
    // Not remembered: the first workspace next time.
  }
}

// A push opens /notification/{id}: read as the signed-in person and followed to where it leads,
// outside the shell, which would take that address for an unknown route.
// `onMe` takes the account's new state when the shell changed it (a workspace was created).
export function Shell({ me, onLogout, onMe }: { me: Me; onLogout: () => void; onMe?: (me: Me) => void }) {
  const path = usePath()
  const opening = parseNotificationPath(path)
  if (path === EMAIL_CONFIRM_PATH) {
    return <ConfirmEmail token={window.location.hash.slice(1)} workspace={me.workspaces[0]?.id ?? null} />
  }
  if (opening !== null) {
    return <OpenNotification id={opening} fallbackWorkspace={me.workspaces[0]?.id ?? null} />
  }
  return <Frame me={me} onLogout={onLogout} onMe={onMe} />
}

function Frame({ me, onLogout, onMe }: { me: Me; onLogout: () => void; onMe?: (me: Me) => void }) {
  const path = usePath()
  usePushSync(me.user.id, me.workspaces.length > 0)
  const parsed = parseRoute(path)
  const member = (id: number) => me.workspaces.some((w) => w.id === id)
  const fallback = me.workspaces.find((w) => w.id === lastWorkspace()) ?? me.workspaces[0]
  const route: Route | null =
    parsed && member(parsed.workspace)
      ? parsed
      : fallback
        ? { workspace: fallback.id, section: parsed?.section ?? 'conversations' }
        : null
  const [drawer, setDrawer] = useState(false)
  const drawerOpen = useRef(false)
  drawerOpen.current = drawer
  const menuButton = useRef<HTMLButtonElement>(null)
  const sidebar = useRef<HTMLElement>(null)
  const main = useRef<HTMLElement>(null)
  const [theme, setTheme] = useTheme()
  const t = useT()

  const target = route ? routePath(route) : null
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
  // click on the menu button before it would be undone.
  const drawnAt = useRef(path)
  useEffect(() => {
    if (drawnAt.current === path) return
    drawnAt.current = path
    if (drawerOpen.current) focusPage.current = true
    setDrawer(false)
  }, [path])
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
      if (e.key !== 'Escape') return
      setDrawer(false)
      menuButton.current?.focus()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [drawer])

  if (!route) {
    return (
      <div className="tt-center">
        <p>{t('shell.noWorkspace')}</p>
        {me.can_create_workspace && onMe && (
          <NewWorkspace
            user={me.user.id}
            onCreated={(next, created) => {
              onMe(next)
              navigate(routePath({ workspace: created.id, section: 'conversations' }))
            }}
          />
        )}
        <button type="button" className="tt-button" onClick={onLogout}>
          {t('shell.logOut')}
        </button>
      </div>
    )
  }

  const workspace = me.workspaces.find((w) => w.id === route.workspace) ?? me.workspaces[0]
  const section = NAV.find((n) => n.section === route.section)

  return (
    <div className="tt-shell" data-drawer={drawer ? 'open' : 'closed'}>
      <button type="button" className="tt-skip" onClick={() => main.current?.focus()}>
        {t('shell.skipToContent')}
      </button>
      <header className="tt-topbar">
        <button
          ref={menuButton}
          type="button"
          className="tt-icon-button tt-menu-button"
          aria-label={drawer ? t('shell.closeMenu') : t('shell.openMenu')}
          aria-expanded={drawer}
          aria-controls="tt-sidebar"
          onClick={() => setDrawer((d) => !d)}
        >
          <Icon name="menu" />
        </button>
        <span className="tt-brand">
          <BrandLogo name={BRAND} />
        </span>
        <label className="tt-workspace">
          <span className="tt-sr-only">{t('shell.workspace')}</span>
          {/* The name is cut to the room the bar has; the whole of it is the tooltip. */}
          <select
            value={workspace.id}
            title={workspace.name}
            onChange={(e) => navigate(routePath({ workspace: Number(e.target.value), section: route.section }))}
          >
            {me.workspaces.map((w) => (
              <option key={w.id} value={w.id}>
                {w.name}
              </option>
            ))}
          </select>
        </label>
        {me.can_create_workspace && onMe && (
          <NewWorkspace
            user={me.user.id}
            onCreated={(next, created) => {
              // The account first, so the new workspace is one the shell knows when the address names it.
              onMe(next)
              navigate(routePath({ workspace: created.id, section: 'conversations' }))
            }}
          />
        )}
        <span className="tt-topbar-section">
          {section ? t(section.label) : route.section === 'notifications' ? t('notifications.title') : ''}
        </span>
        <div className="tt-topbar-end">
          <Bell workspace={workspace.id} current={route.section === 'notifications'} t={t} />
          <ThemeSelect theme={theme} onChange={setTheme} t={t} />
          <AccountMenu me={me} workspace={workspace.id} onLogout={onLogout} t={t} />
        </div>
      </header>
      <div className="tt-body">
        <nav id="tt-sidebar" ref={sidebar} className="tt-sidebar" aria-label={t('shell.sectionsNav')}>
          {NAV.map((n) => (
            <Link
              key={n.section}
              href={routePath({ workspace: workspace.id, section: n.section })}
              className="tt-nav-link"
              aria-current={n.section === route.section ? 'page' : undefined}
            >
              <Icon name={n.section} />
              <span>{t(n.label)}</span>
            </Link>
          ))}
          {/* Pinned apps are listed after the platform's pages, in a section of their own: they can't
              replace, reorder or hide those, and a failure here leaves the navigation as it is. */}
          <ErrorBoundary fallback={null} resetKey={workspace.id}>
            <PinnedNav workspace={workspace.id} path={path} />
          </ErrorBoundary>
        </nav>
        {drawer && (
          <button
            type="button"
            className="tt-scrim"
            aria-label={t('shell.closeMenu')}
            onClick={() => setDrawer(false)}
          />
        )}
        <main
          id="tt-main"
          ref={main}
          tabIndex={-1}
          className="tt-page"
          inert={drawer || undefined}
          key={`${workspace.id}/${route.section}`}
        >
          {/* A page that fails to draw leaves the shell around it (the menu, the settings) usable. */}
          <ErrorBoundary fallback={<PageFailed />} resetKey={path}>
            <Page route={route} me={me} workspace={workspace} />
          </ErrorBoundary>
        </main>
      </div>
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
    <div className="tt-section">
      <p role="alert" className="tt-error">
        {t('shell.pageFailed')}
      </p>
      <button type="button" className="tt-button" onClick={() => window.location.reload()}>
        {t('shell.reload')}
      </button>
    </div>
  )
}

function Loading() {
  const t = useT()
  return <p className="tt-section tt-muted">{t('app.loading')}</p>
}

// The inbox's link in the top bar, with how many notifications are unread. The count is a
// convenience: a failed ask leaves it as it was.
function Bell({ workspace, current, t }: { workspace: number; current: boolean; t: T }) {
  const unread = useUnread(workspace)
  const f = useFormat()
  return (
    <Link
      href={routePath({ workspace, section: 'notifications' })}
      className="tt-icon-button tt-bell"
      aria-current={current ? 'page' : undefined}
      aria-label={unread > 0 ? t('notifications.bellUnread', { count: unread }) : t('notifications.bell')}
    >
      <Icon name="bell" />
      {unread > 0 && (
        <span className="tt-badge" aria-hidden>
          {unread > 99 ? `${f.number(99)}+` : f.number(unread)}
        </span>
      )}
    </Link>
  )
}

function ThemeSelect({ theme, onChange, t }: { theme: Theme; onChange: (theme: Theme) => void; t: T }) {
  return (
    <label className="tt-theme">
      <span className="tt-sr-only">{t('shell.theme.label')}</span>
      <Icon name={theme === 'dark' ? 'moon' : theme === 'light' ? 'sun' : 'contrast'} />
      <select value={theme} onChange={(e) => onChange(e.target.value as Theme)}>
        <option value="system">{t('shell.theme.system')}</option>
        <option value="light">{t('shell.theme.light')}</option>
        <option value="dark">{t('shell.theme.dark')}</option>
      </select>
    </label>
  )
}

// Whether a form control's list is open (the `:open` state: a <select>'s picker). A browser
// without it never says so, and the control behaves as it did.
function isOpen(el: Element): boolean {
  try {
    return el.matches(':open')
  } catch {
    return false
  }
}

function AccountMenu({ me, workspace, onLogout, t }: { me: Me; workspace: number; onLogout: () => void; t: T }) {
  const { name, email } = me.user
  const { installed } = useInstall()
  const menu = useRef<HTMLDetailsElement>(null)
  const summary = useRef<HTMLElement>(null)
  // A <details> only closes on its summary: Escape, and a press or focus outside it, close it
  // too, and Escape gives focus back to the summary, which the menu's items had. The listeners
  // are on the document only while the menu is open.
  const [open, setOpen] = useState(false)
  useEffect(() => {
    if (!open) return
    const close = () => menu.current?.removeAttribute('open')
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented || !menu.current?.open) return
      // A language list that is open takes this Escape to close itself: the menu stays, and the
      // next Escape closes it.
      if (e.target instanceof HTMLSelectElement && isOpen(e.target)) return
      close()
      summary.current?.focus()
    }
    const onOutside = (e: Event) => {
      if (menu.current?.open && e.target instanceof Node && !menu.current.contains(e.target)) close()
    }
    document.addEventListener('keydown', onKey)
    document.addEventListener('pointerdown', onOutside)
    document.addEventListener('focusin', onOutside)
    return () => {
      document.removeEventListener('keydown', onKey)
      document.removeEventListener('pointerdown', onOutside)
      document.removeEventListener('focusin', onOutside)
    }
  }, [open])
  return (
    <details className="tt-account" ref={menu} onToggle={(e) => setOpen(e.currentTarget.open)}>
      <summary ref={summary} aria-label={t('shell.account', { name })}>
        <span className="tt-avatar" aria-hidden>
          {name.trim().charAt(0).toUpperCase() || '?'}
        </span>
      </summary>
      <div className="tt-menu">
        <p>
          <strong>{name}</strong>
          <br />
          <span className="tt-muted">{email}</span>
        </p>
        <LanguageSelect initial={me.user.locale ?? ''} t={t} />
        {!installed && (
          <Link
            className="tt-button"
            href={routePath({ workspace, section: 'settings', item: 'about' })}
            onClick={() => menu.current?.removeAttribute('open')}
          >
            {t('shell.installApp')}
          </Link>
        )}
        <button type="button" className="tt-button" onClick={onLogout}>
          {t('shell.logOut')}
        </button>
      </div>
    </details>
  )
}

// The user's language: one the app has, or automatic (the browser's, else the workspace's).
// Saved to their account, so the agent and dictation follow it too.
function LanguageSelect({ initial, t }: { initial: string; t: T }) {
  const [choice, setChoice] = useState(initial)
  const [error, setError] = useState<string | null>(null)
  useLocale() // re-rendered on a change, so the automatic one's name follows the workspace's default
  // The pick the account has, and the latest pick asked for. One save is in flight at a
  // time: when it ends, the latest pick is sent if the account doesn't have it yet, so
  // quick picks can't reach the server out of order and leave an older one saved. A pick
  // the server refused is undone, so this browser never shows a language the account
  // doesn't have.
  const saved = useRef(initial)
  const wanted = useRef(initial)
  const saving = useRef(false)
  // A new value from the server (the account's /api/me again) is the one to show and to go
  // back to, unless a pick is still being saved: that value is from before the pick, which
  // stays the one wanted and shown. A value the account already has (this effect's first run,
  // which can come after a pick made at once, or /api/me confirming the pick just saved) is
  // nothing new: acting on it would undo that pick.
  useEffect(() => {
    if (initial === saved.current) return
    saved.current = initial
    if (saving.current) return
    wanted.current = initial
    setChoice(initial)
  }, [initial])
  const save = async () => {
    saving.current = true
    try {
      while (wanted.current !== saved.current) {
        const tag = wanted.current
        try {
          await api('/api/me', { method: 'PATCH', body: { locale: tag } })
          saved.current = tag
        } catch (e) {
          if (wanted.current !== tag) continue // a later pick replaced it: send that one
          wanted.current = saved.current
          setError(errorText(e))
          setChoice(saved.current)
          await setPreferences({ user: saved.current || null })
        }
      }
    } finally {
      saving.current = false
    }
  }
  const pick = async (tag: string) => {
    wanted.current = tag
    setChoice(tag)
    setError(null)
    await setPreferences({ user: tag || null })
    if (!saving.current) await save()
  }
  return (
    <div className="tt-language">
      <label className="tt-language">
        <span>{t('language.label')}</span>
        <select value={choice} onChange={(e) => pick(e.target.value)}>
          <option value="">{t('language.automatic', { name: localeName(automaticLocale()) })}</option>
          {/* A pick the app has no file for (made through the API, for the agent) is listed too. */}
          {(choice && !LOCALES.includes(choice) ? [...LOCALES, choice] : LOCALES).map((tag) => (
            <option key={tag} value={tag} lang={tag}>
              {localeName(tag)}
            </option>
          ))}
        </select>
      </label>
      {error && (
        <span role="alert" className="tt-error tt-small">
          {t('language.notSaved', { error })}
        </span>
      )}
    </div>
  )
}
