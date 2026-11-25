// A signed-in person who is a member of no workspace: what that means, a new workspace if the
// server lets them make one (a dialog, which fits a phone), and Log out.

import { useState } from 'react'
import type { Me } from '../api'
import { AuthLayout } from '../auth/AuthLayout'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { NewWorkspaceDialog } from './NewWorkspace'
import './NoWorkspace.css'

export function NoWorkspace({ me, onLogout, onMe }: { me: Me; onLogout: () => void; onMe?: (me: Me) => void }) {
  const t = useT()
  const [creating, setCreating] = useState(false)
  const canCreate = Boolean(me.can_create_workspace && onMe)
  return (
    <AuthLayout>
      <EmptyState
        level={1}
        title={t('shell.noWorkspaceTitle')}
        actions={
          <>
            {canCreate && (
              <Button variant="primary" icon="plus" onClick={() => setCreating(true)}>
                {t('shell.newWorkspace')}
              </Button>
            )}
            <Button onClick={onLogout}>{t('shell.logOut')}</Button>
          </>
        }
      >
        <p>{t('shell.noWorkspace')}</p>
      </EmptyState>
      {canCreate && onMe && (
        <NewWorkspaceDialog open={creating} onClose={() => setCreating(false)} user={me.user.id} onMe={onMe} />
      )}
    </AuthLayout>
  )
}
