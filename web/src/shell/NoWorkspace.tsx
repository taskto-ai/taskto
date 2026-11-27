import { useState } from 'react'
import type { Me } from '../api'
import { AuthLayout } from '../auth/AuthLayout'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { NewWorkspaceDialog } from './NewWorkspace'

export function NoWorkspace({ me, onLogout, onMe }: { me: Me; onLogout: () => void; onMe?: (me: Me) => void }) {
  const t = useT()
  const [creating, setCreating] = useState(false)
  const canCreate = Boolean(me.can_create_workspace && onMe)
  return (
    <AuthLayout title={t('shell.noWorkspaceTitle')} help={<p>{t('shell.noWorkspace')}</p>}>
      <div className="ui-auth-stack">
        {canCreate && (
          <Button variant="primary" icon="plus" onClick={() => setCreating(true)}>
            {t('shell.newWorkspace')}
          </Button>
        )}
        <Button onClick={onLogout}>{t('shell.logOut')}</Button>
      </div>
      {canCreate && onMe && (
        <NewWorkspaceDialog open={creating} onClose={() => setCreating(false)} user={me.user.id} onMe={onMe} />
      )}
    </AuthLayout>
  )
}
