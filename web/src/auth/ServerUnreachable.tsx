// The server didn't answer, and nobody signed in on this device before (else the offline shell
// opens): what happened, what was said, and Try again. In the frame's own form, its title in the
// display face like every screen there, not a box of its own inside the card. The title is inside
// the alert, so what is announced starts with what happened.

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
