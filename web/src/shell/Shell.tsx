// The signed-in layout: a top bar (workspace, theme, account), the sidebar with the
// workspace's sections, and the page. On a phone the sidebar is a drawer behind the menu
// button. The URL names the workspace and section (/w/{workspace}/{section}); a URL that
// names neither, or a workspace the user isn't in, goes to the last workspace used.

import { lazy, type ReactNode, Suspense, useEffect, useRef, useState } from 'react'
import { api, errorText, type Me, type Workspace } from '../api'
import { BoardsPage } from '../boards/BoardsPage'
import { ConversationsPage } from '../conversations/ConversationsPage'
import { automaticLocale, type Key, LOCALES, localeName, setPreferences, type T, useLocale, useT } from '../i18n'
import { KnowledgePage } from '../knowledge/KnowledgePage'
import { useInstall } from '../pwa/install'
import { Link, navigate, parseRoute, type Route, routePath, type Section, usePath } from '../router'
import { SettingsPage } from '../settings/SettingsPage'
import { type Theme, useTheme } from '../theme'
import { Icon } from './Icon'

// The A2UI renderer is most of the app's code: loaded when Apps is opened.
const AppsPage = lazy(() => import('../apps/AppsPage').then((m) => ({ default: m.AppsPage })))

const LAST_WORKSPACE = 'taskto.workspace'

const NAV: { section: Section; label: Key }[] = [
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

export function Shell({ me, onLogout }: { me: Me; onLogout: () => void }) {
  const path = usePath()
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
  // biome-ignore lint/correctness/useExhaustiveDependencies: closes on every path change
  useEffect(() => {
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
    sidebar.current?.querySelector<HTMLElement>('[aria-current="page"], a')?.focus()
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
        <span className="tt-brand">{BRAND}</span>
        <label className="tt-workspace">
          <span className="tt-sr-only">{t('shell.workspace')}</span>
          <select
            value={workspace.id}
            onChange={(e) => navigate(routePath({ workspace: Number(e.target.value), section: route.section }))}
          >
            {me.workspaces.map((w) => (
              <option key={w.id} value={w.id}>
                {w.name}
              </option>
            ))}
          </select>
        </label>
        <span className="tt-topbar-section">{section && t(section.label)}</span>
        <div className="tt-topbar-end">
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
          <Page route={route} me={me} workspace={workspace} />
        </main>
      </div>
    </div>
  )
}

function Page({ route, me, workspace }: { route: Route; me: Me; workspace: Workspace }): ReactNode {
  switch (route.section) {
    case 'conversations':
      return <ConversationsPage workspace={route.workspace} open={route.item} />
    case 'knowledge':
      return <KnowledgePage workspace={route.workspace} admin={workspace.role === 'admin'} />
    case 'boards':
      return <BoardsPage workspace={route.workspace} />
    case 'apps':
      return (
        <Suspense fallback={<Loading />}>
          <AppsPage workspace={route.workspace} open={route.item} />
        </Suspense>
      )
    case 'settings':
      return <SettingsPage me={me} workspace={workspace} tab={route.item} />
  }
}

function Loading() {
  const t = useT()
  return <p className="tt-section tt-muted">{t('app.loading')}</p>
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

function AccountMenu({ me, workspace, onLogout, t }: { me: Me; workspace: number; onLogout: () => void; t: T }) {
  const { name, email } = me.user
  const { installed } = useInstall()
  return (
    <details className="tt-account">
      <summary aria-label={t('shell.account', { name })}>
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
            onClick={(e) => e.currentTarget.closest('details')?.removeAttribute('open')}
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
  // stays the one wanted and shown.
  useEffect(() => {
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
