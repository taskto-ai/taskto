// A signed-in person who is a member of no workspace: what that means, a new workspace if the
// server lets them make one (the shell's one "New workspace" dialog), and Log out.

import { useState } from 'react'
import type { Me } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { Page } from '../ui/Page'
import { NewWorkspaceDialog } from './NewWorkspace'

export function NoWorkspace({ me, onLogout, onMe }: { me: Me; onLogout: () => void; onMe?: (me: Me) => void }) {
  const t = useT()
  const [creating, setCreating] = useState(false)
  return (
    <Page width="reading" className="ui-shell-alone">
      <EmptyState
        title={t('shell.noWorkspaceTitle')}
        actions={
          <>
            {me.can_create_workspace && onMe && (
              <Button variant="primary" icon="plus" onClick={() => setCreating(true)}>
                {t('shell.newWorkspace')}
              </Button>
            )}
            <Button onClick={onLogout}>{t('shell.logOut')}</Button>
          </>
        }
      >
        {t('shell.noWorkspace')}
      </EmptyState>
      {me.can_create_workspace && onMe && (
        <NewWorkspaceDialog open={creating} onClose={() => setCreating(false)} user={me.user.id} onMe={onMe} />
      )}
    </Page>
  )
}
