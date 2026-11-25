// The server didn't answer, and nobody signed in on this device before (else the offline shell
// opens): what happened, what was said, and Try again.

import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { Icon } from '../ui/Icon'
import { AuthLayout } from './AuthLayout'

export function ServerUnreachable({ error, onRetry }: { error: string; onRetry: () => void }) {
  const t = useT()
  return (
    <AuthLayout>
      <div role="alert">
        <EmptyState
          icon={<Icon name="offline" size={24} />}
          level={1}
          title={t('app.unreachableTitle')}
          actions={
            <Button variant="primary" onClick={onRetry}>
              {t('app.tryAgain')}
            </Button>
          }
        >
          <p>{t('app.unreachableHelp')}</p>
          <p className="ui-auth-note">{t('app.unreachableDetail', { error })}</p>
        </EmptyState>
      </div>
    </AuthLayout>
  )
}
