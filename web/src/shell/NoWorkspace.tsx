// A signed-in person who is a member of no workspace: what that means, a new workspace if the
// server lets them make one, and Log out.

import type { Me } from '../api'
import { AuthLayout } from '../auth/AuthLayout'
import { useT } from '../i18n'
import { navigate, routePath } from '../router'
import { Button } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { NewWorkspace } from './NewWorkspace'

export function NoWorkspace({ me, onLogout, onMe }: { me: Me; onLogout: () => void; onMe?: (me: Me) => void }) {
  const t = useT()
  return (
    <AuthLayout>
      <EmptyState
        title={t('shell.noWorkspaceTitle')}
        actions={
          <>
            {me.can_create_workspace && onMe && (
              <NewWorkspace
                user={me.user.id}
                onCreated={(next, created) => {
                  onMe(next)
                  navigate(routePath({ workspace: created.id, section: 'conversations' }))
                }}
              />
            )}
            <Button onClick={onLogout}>{t('shell.logOut')}</Button>
          </>
        }
      >
        <p>{t('shell.noWorkspaceHelp')}</p>
      </EmptyState>
    </AuthLayout>
  )
}
