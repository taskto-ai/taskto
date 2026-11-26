// Signing out, once it is certain: the shell is gone at once (nothing of it can be pressed while
// the sign-out finishes), and this says what is happening. A sign-out the server didn't confirm (no
// answer, or its own error) leaves the person signed in: they are told so, and can try again or go
// back to the app.

import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { Icon } from '../ui/Icon'
import { Spinner } from '../ui/Spinner'
import { AuthLayout } from './AuthLayout'

export function SigningOut({
  failed,
  busy = false,
  onRetry,
  onBack,
}: {
  // What went wrong, when the server didn't confirm the sign-out.
  failed?: string
  // Back is asking who is signed in: it says so, and neither button can be pressed meanwhile.
  busy?: boolean
  onRetry: () => void
  onBack: () => void
}) {
  const t = useT()
  if (failed === undefined) {
    return (
      <AuthLayout busy>
        <div role="status" className="ui-auth-signing-out">
          <Spinner />
          <p>{t('app.signingOut')}</p>
        </div>
      </AuthLayout>
    )
  }
  return (
    <AuthLayout>
      <div role="alert">
        <EmptyState
          icon={<Icon name="alert" size={24} />}
          level={1}
          title={t('app.signOutFailedTitle')}
          actions={
            <>
              <Button variant="primary" onClick={onRetry} disabled={busy}>
                {t('app.tryAgain')}
              </Button>
              <Button onClick={onBack} loading={busy}>
                {t('app.backToApp')}
              </Button>
            </>
          }
        >
          <p>{t('app.signOutFailedHelp')}</p>
          <p className="ui-auth-note">{t('app.unreachableDetail', { error: failed })}</p>
        </EmptyState>
      </div>
    </AuthLayout>
  )
}
