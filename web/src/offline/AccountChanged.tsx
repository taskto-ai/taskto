// Another account is signed in on this browser than the one this tab was showing (someone signed
// in, or out, in another tab). Nothing of the earlier account's is on screen or in memory any more:
// this says only that, and the page is reloaded to go on as whoever is signed in (docs/offline.md).

import { AuthLayout } from '../auth/AuthLayout'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Dialog } from '../ui/Dialog'

export function AccountChanged() {
  const t = useT()
  return (
    // Behind it, the logo only: nothing of the earlier account.
    <AuthLayout>
      <Dialog
        open
        // Reloading is the only way on.
        dismissible={false}
        onClose={() => undefined}
        role="alertdialog"
        size="sm"
        title={t('offline.accountChanged.title')}
        actions={
          <Button variant="primary" onClick={() => window.location.reload()} data-autofocus>
            {t('offline.accountChanged.reload')}
          </Button>
        }
      >
        <p>{t('offline.accountChanged.body')}</p>
      </Dialog>
    </AuthLayout>
  )
}
