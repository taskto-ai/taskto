// The workspace switcher, at the top of the sidebar: the current workspace (a square avatar and its
// name) on a button that opens a menu of the person's workspaces, and "New workspace" when the
// server lets them make one. A long name is cut with an ellipsis; the whole of it is the button's
// tooltip and is in its name. Picking a workspace keeps the section the person is in.

import { useState } from 'react'
import type { Me, Workspace } from '../api'
import type { T } from '../i18n'
import { navigate, routePath, type Section } from '../router'
import { Avatar } from '../ui/Avatar'
import { Menu, MenuButton, MenuGroup, MenuItem, MenuList, MenuSeparator } from '../ui/Menu'
import { Tooltip } from '../ui/Tooltip'
import { NewWorkspaceDialog } from './NewWorkspace'

export interface WorkspaceSwitcherProps {
  me: Me
  workspace: Workspace
  section: Section
  // The account's new state, when a workspace was made; absent, "New workspace" isn't offered.
  onMe?: (me: Me) => void
  t: T
}

export function WorkspaceSwitcher({ me, workspace, section, onMe, t }: WorkspaceSwitcherProps) {
  const [creating, setCreating] = useState(false)
  const canCreate = Boolean(me.can_create_workspace && onMe)
  return (
    <div className="ui-shell-switcher">
      <Menu align="start">
        <Tooltip content={workspace.name} describes={false}>
          <MenuButton
            label={t('shell.workspaceCurrent', { name: workspace.name })}
            indicator="chevron-updown"
            className="ui-shell-switcher-button"
          >
            <Avatar name={workspace.name} shape="square" size="sm" decorative />
            <span className="ui-shell-switcher-name">{workspace.name}</span>
          </MenuButton>
        </Tooltip>
        <MenuList>
          <MenuGroup label={t('shell.workspaces')}>
            {me.workspaces.map((w) => (
              <MenuItem
                key={w.id}
                checked={w.id === workspace.id}
                onSelect={() => {
                  if (w.id !== workspace.id) navigate(routePath({ workspace: w.id, section }))
                }}
              >
                {w.name}
              </MenuItem>
            ))}
          </MenuGroup>
          {canCreate && <MenuSeparator />}
          {canCreate && (
            <MenuItem icon="plus" onSelect={() => setCreating(true)}>
              {t('shell.newWorkspace')}
            </MenuItem>
          )}
        </MenuList>
      </Menu>
      {canCreate && onMe && (
        <NewWorkspaceDialog
          open={creating}
          onClose={() => setCreating(false)}
          user={me.user.id}
          onCreated={(next, created) => {
            // The account first, so the new workspace is one the shell knows when the address names it.
            onMe(next)
            navigate(routePath({ workspace: created.id, section: 'conversations' }))
          }}
        />
      )}
    </div>
  )
}
