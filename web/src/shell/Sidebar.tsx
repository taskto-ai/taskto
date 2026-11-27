import type { Ref } from 'react'
import type { Me, Workspace } from '../api'
import { BrandLogo } from '../BrandLogo'
import { PinnedNav } from '../home/PinnedNav'
import { type T, useFormat } from '../i18n'
import { useUnread } from '../notifications/useUnread'
import { type Route, routePath } from '../router'
import { Icon } from '../ui/Icon'
import { AccountMenu } from './AccountMenu'
import { ErrorBoundary } from './ErrorBoundary'
import { NavLink } from './NavLink'
import { BRAND, NAV, SIDEBAR_ID } from './nav'
import { WorkspaceSwitcher } from './WorkspaceSwitcher'

export interface SidebarProps {
  me: Me
  workspace: Workspace
  route: Route
  path: string
  onLogout: () => void
  onMe?: (me: Me) => void
  sidebarRef: Ref<HTMLDivElement>
  t: T
}

export function Sidebar({ me, workspace, route, path, onLogout, onMe, sidebarRef, t }: SidebarProps) {
  return (
    <div id={SIDEBAR_ID} ref={sidebarRef} className="ui-shell-sidebar">
      <span className="ui-shell-logo">
        <BrandLogo name={BRAND} />
      </span>
      <WorkspaceSwitcher me={me} workspace={workspace} section={route.section} onMe={onMe} t={t} />
      <nav className="ui-shell-nav" aria-label={t('shell.sectionsNav')}>
        {NAV.map((n) => (
          <NavLink
            key={n.section}
            href={routePath({ workspace: workspace.id, section: n.section })}
            current={n.section === route.section}
            leading={<Icon name={n.icon} size={18} />}
            label={t(n.label)}
          />
        ))}
        <InboxLink workspace={workspace.id} current={route.section === 'notifications'} t={t} />
        <ErrorBoundary fallback={null} resetKey={workspace.id}>
          <PinnedNav workspace={workspace.id} path={path} />
        </ErrorBoundary>
        <span className="ui-shell-nav-end">
          <NavLink
            href={routePath({ workspace: workspace.id, section: 'settings' })}
            current={route.section === 'settings'}
            leading={<Icon name="settings" size={18} />}
            label={t('shell.sections.settings')}
          />
        </span>
      </nav>
      <AccountMenu me={me} workspace={workspace.id} onLogout={onLogout} t={t} />
    </div>
  )
}

function InboxLink({ workspace, current, t }: { workspace: number; current: boolean; t: T }) {
  const unread = useUnread(workspace)
  const f = useFormat()
  return (
    <NavLink
      href={routePath({ workspace, section: 'notifications' })}
      current={current}
      leading={<Icon name="bell" size={18} />}
      label={t('notifications.title')}
      name={unread > 0 ? t('notifications.bellUnread', { count: unread }) : undefined}
      trailing={
        unread > 0 && (
          <span className="ui-shell-count" aria-hidden="true">
            {unread > 99 ? `${f.number(99)}+` : f.number(unread)}
          </span>
        )
      }
    />
  )
}
