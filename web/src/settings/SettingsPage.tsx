import type { Me, Workspace } from '../api'
import { type Key, useT } from '../i18n'
import { Link, routePath, useNavigate } from '../router'
import { Select } from '../ui/Select'
import { AboutTab } from './AboutTab'
import { ApprovalsTab } from './ApprovalsTab'
import { AuditTab } from './AuditTab'
import { CliAccountsTab } from './CliAccountsTab'
import { DataTab } from './DataTab'
import { DictationTab } from './DictationTab'
import { IntegrationEventsTab } from './EventsTab'
import { GeneralTab } from './GeneralTab'
import { IntegrationsTab } from './IntegrationsTab'
import { MembersTab } from './MembersTab'
import { NotificationsTab } from './NotificationsTab'
import { PasswordTab } from './PasswordTab'
import { ProvidersTab } from './ProvidersTab'
import { SearchTab } from './SearchTab'
import { SessionsTab } from './SessionsTab'
import { SpacesTab } from './SpacesTab'
import { StorageTab } from './StorageTab'
import { TokensTab } from './TokensTab'
import './Settings.css'

type Tab =
  | 'general'
  | 'members'
  | 'spaces'
  | 'audit'
  | 'providers'
  | 'integrations'
  | 'integration-approvals'
  | 'integration-events'
  | 'cli-accounts'
  | 'search'
  | 'dictation'
  | 'notifications'
  | 'password'
  | 'sessions'
  | 'data'
  | 'tokens'
  | 'storage'
  | 'about'

type Group = 'workspace' | 'you' | 'server'

const TABS: { tab: Tab; label: Key; admin: boolean; group: Group }[] = [
  { tab: 'general', label: 'settings.tabs.general', admin: true, group: 'workspace' },
  { tab: 'members', label: 'settings.tabs.members', admin: true, group: 'workspace' },
  { tab: 'spaces', label: 'settings.tabs.spaces', admin: true, group: 'workspace' },
  { tab: 'audit', label: 'settings.tabs.audit', admin: true, group: 'workspace' },
  { tab: 'providers', label: 'settings.tabs.providers', admin: false, group: 'workspace' },
  { tab: 'integrations', label: 'settings.tabs.integrations', admin: true, group: 'workspace' },
  { tab: 'integration-approvals', label: 'settings.tabs.integrationApprovals', admin: true, group: 'workspace' },
  { tab: 'integration-events', label: 'settings.tabs.integrationEvents', admin: true, group: 'workspace' },
  { tab: 'cli-accounts', label: 'settings.tabs.cliAccounts', admin: false, group: 'workspace' },
  { tab: 'search', label: 'settings.tabs.search', admin: false, group: 'workspace' },
  { tab: 'dictation', label: 'settings.tabs.dictation', admin: false, group: 'workspace' },
  { tab: 'storage', label: 'settings.tabs.storage', admin: false, group: 'workspace' },
  { tab: 'notifications', label: 'settings.tabs.notifications', admin: false, group: 'you' },
  { tab: 'password', label: 'settings.tabs.password', admin: false, group: 'you' },
  { tab: 'sessions', label: 'settings.tabs.sessions', admin: false, group: 'you' },
  { tab: 'data', label: 'settings.tabs.data', admin: false, group: 'you' },
  { tab: 'tokens', label: 'settings.tabs.tokens', admin: false, group: 'you' },
  { tab: 'about', label: 'settings.tabs.about', admin: false, group: 'server' },
]

const GROUPS: { group: Group; label: Key }[] = [
  { group: 'workspace', label: 'settings.groups.workspace' },
  { group: 'you', label: 'settings.groups.you' },
  { group: 'server', label: 'settings.groups.server' },
]

export function SettingsPage({ me, workspace, tab }: { me: Me; workspace: Workspace; tab?: string }) {
  const t = useT()
  const go = useNavigate()
  const admin = workspace.role === 'admin'
  const tabs = TABS.filter((x) => admin || !x.admin)
  const current = tabs.find((x) => x.tab === tab) ?? tabs[0]
  const href = (tab: Tab) => routePath({ workspace: workspace.id, section: 'settings', item: tab })
  return (
    <div className="settings-layout">
      <div className="settings-frame">
        <nav className="settings-nav" aria-label={t('settings.title')}>
          <p className="settings-nav-title">{t('settings.title')}</p>
          {GROUPS.map((g) => {
            const items = tabs.filter((x) => x.group === g.group)
            if (items.length === 0) return null
            const id = `settings-group-${g.group}`
            return (
              <div key={g.group}>
                <p className="settings-nav-label" id={id}>
                  {t(g.label)}
                </p>
                <ul className="settings-nav-group" aria-labelledby={id}>
                  {items.map((x) => (
                    <li key={x.tab}>
                      <Link
                        className="settings-nav-link"
                        href={href(x.tab)}
                        aria-current={x === current ? 'page' : undefined}
                      >
                        {t(x.label)}
                      </Link>
                    </li>
                  ))}
                </ul>
              </div>
            )
          })}
        </nav>
        <div className="settings-picker">
          <Select
            aria-label={t('settings.choose')}
            value={current.tab}
            onChange={(e) => go(href(e.target.value as Tab))}
          >
            {GROUPS.map((g) => {
              const items = tabs.filter((x) => x.group === g.group)
              if (items.length === 0) return null
              return (
                <optgroup key={g.group} label={t(g.label)}>
                  {items.map((x) => (
                    <option key={x.tab} value={x.tab}>
                      {t(x.label)}
                    </option>
                  ))}
                </optgroup>
              )
            })}
          </Select>
        </div>
        <Panel tab={current.tab} me={me} workspace={workspace} />
      </div>
    </div>
  )
}

function Panel({ tab, me, workspace }: { tab: Tab; me: Me; workspace: Workspace }) {
  switch (tab) {
    case 'general':
      return <GeneralTab workspace={workspace} email={me.user.email} operator={me.is_operator === true} />
    case 'members':
      return <MembersTab workspace={workspace.id} me={me.user.id} operator={me.is_operator === true} />
    case 'spaces':
      return <SpacesTab workspace={workspace.id} />
    case 'audit':
      return <AuditTab workspace={workspace.id} />
    case 'storage':
      return <StorageTab workspace={workspace.id} admin={workspace.role === 'admin'} />
    case 'providers':
      return <ProvidersTab workspace={workspace.id} admin={workspace.role === 'admin'} />
    case 'integrations':
      return <IntegrationsTab workspace={workspace.id} />
    case 'integration-approvals':
      return <ApprovalsTab workspace={workspace.id} />
    case 'integration-events':
      return <IntegrationEventsTab workspace={workspace.id} />
    case 'cli-accounts':
      return <CliAccountsTab workspace={workspace.id} admin={workspace.role === 'admin'} />
    case 'search':
      return <SearchTab workspace={workspace.id} admin={workspace.role === 'admin'} />
    case 'dictation':
      return <DictationTab workspace={workspace.id} admin={workspace.role === 'admin'} />
    case 'notifications':
      return <NotificationsTab me={me.user.id} workspace={workspace.id} timeZone={me.user.time_zone ?? ''} />
    case 'password':
      return <PasswordTab />
    case 'sessions':
      return <SessionsTab />
    case 'data':
      return <DataTab workspace={workspace.id} user={me.user.id} admin={workspace.role === 'admin'} />
    case 'tokens':
      return <TokensTab workspace={workspace.id} />
    case 'about':
      return <AboutTab workspace={workspace.id} operator={workspace.role === 'admin' && me.is_operator === true} />
  }
}
