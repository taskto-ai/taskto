import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Heading } from '../ui/Heading'
import { AuthLayout } from './AuthLayout'

export function ServerUnreachable({ error, onRetry }: { error: string; onRetry: () => void }) {
  const t = useT()
  return (
    <AuthLayout>
      <div role="alert" className="ui-auth-head">
        <Heading level={1} size="display">
          {t('app.unreachableTitle')}
        </Heading>
        <div className="ui-auth-help">
          <p>{t('app.unreachableHelp')}</p>
          <p className="ui-auth-note">{t('app.unreachableDetail', { error })}</p>
        </div>
      </div>
      <div className="ui-auth-stack">
        <Button variant="primary" onClick={onRetry}>
          {t('app.tryAgain')}
        </Button>
      </div>
    </AuthLayout>
  )
}
