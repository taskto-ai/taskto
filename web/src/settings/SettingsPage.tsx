// The workspace's settings, a tab each: general (its default language), members and invites,
// shared spaces, model providers, CLI accounts, search, dictation, notifications (push on this
// device), your password, access
// tokens and about. Admins see them all; members see model providers (their own accounts, and
// the workspace's to read), their own CLI accounts, search (their spaces' consent), dictation
// (what is in use), notifications, their own password and access tokens, and about (the credits; admins also
// see the server's backups there). The tab is the route's item.

import type { Me, Workspace } from '../api'
import { type Key, useT } from '../i18n'
import { Link, routePath } from '../router'
import { AboutTab } from './AboutTab'
import { CliAccountsTab } from './CliAccountsTab'
import { DictationTab } from './DictationTab'
import { GeneralTab } from './GeneralTab'
import { MembersTab } from './MembersTab'
import { NotificationsTab } from './NotificationsTab'
import { PasswordTab } from './PasswordTab'
import { ProvidersTab } from './ProvidersTab'
import { SearchTab } from './SearchTab'
import { SpacesTab } from './SpacesTab'
import { TokensTab } from './TokensTab'

type Tab =
  | 'general'
  | 'members'
  | 'spaces'
  | 'providers'
  | 'cli-accounts'
  | 'search'
  | 'dictation'
  | 'notifications'
  | 'password'
  | 'tokens'
  | 'about'

const TABS: { tab: Tab; label: Key; admin: boolean }[] = [
  { tab: 'general', label: 'settings.tabs.general', admin: true },
  { tab: 'members', label: 'settings.tabs.members', admin: true },
  { tab: 'spaces', label: 'settings.tabs.spaces', admin: true },
  { tab: 'providers', label: 'settings.tabs.providers', admin: false },
  { tab: 'cli-accounts', label: 'settings.tabs.cliAccounts', admin: false },
  { tab: 'search', label: 'settings.tabs.search', admin: false },
  { tab: 'dictation', label: 'settings.tabs.dictation', admin: false },
  { tab: 'notifications', label: 'settings.tabs.notifications', admin: false },
  { tab: 'password', label: 'settings.tabs.password', admin: false },
  { tab: 'tokens', label: 'settings.tabs.tokens', admin: false },
  { tab: 'about', label: 'settings.tabs.about', admin: false },
]

export function SettingsPage({ me, workspace, tab }: { me: Me; workspace: Workspace; tab?: string }) {
  const t = useT()
  const admin = workspace.role === 'admin'
  const tabs = TABS.filter((x) => admin || !x.admin)
  const current = tabs.find((x) => x.tab === tab) ?? tabs[0]
  return (
    <div className="tt-section">
      <div className="tt-pane-head">
        <h1>{t('settings.title')}</h1>
      </div>
      <nav className="tt-tabs" aria-label={t('settings.title')}>
        {tabs.map((tab) => (
          <Link
            key={tab.tab}
            href={routePath({ workspace: workspace.id, section: 'settings', item: tab.tab })}
            aria-current={tab === current ? 'page' : undefined}
          >
            {t(tab.label)}
          </Link>
        ))}
      </nav>
      <Panel tab={current.tab} me={me} workspace={workspace} />
    </div>
  )
}

function Panel({ tab, me, workspace }: { tab: Tab; me: Me; workspace: Workspace }) {
  switch (tab) {
    case 'general':
      return <GeneralTab workspace={workspace} />
    case 'members':
      return <MembersTab workspace={workspace.id} me={me.user.id} />
    case 'spaces':
      return <SpacesTab workspace={workspace.id} />
    case 'providers':
      return <ProvidersTab workspace={workspace.id} admin={workspace.role === 'admin'} />
    case 'cli-accounts':
      return <CliAccountsTab workspace={workspace.id} admin={workspace.role === 'admin'} />
    case 'search':
      return <SearchTab workspace={workspace.id} admin={workspace.role === 'admin'} />
    case 'dictation':
      return <DictationTab workspace={workspace.id} admin={workspace.role === 'admin'} />
    case 'notifications':
      return <NotificationsTab me={me.user.id} workspace={workspace.id} />
    case 'password':
      return <PasswordTab />
    case 'tokens':
      return <TokensTab workspace={workspace.id} />
    case 'about':
      return <AboutTab workspace={workspace.id} admin={workspace.role === 'admin'} />
  }
}
