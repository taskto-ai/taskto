import { AuthLayout } from '../auth/AuthLayout'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Dialog } from '../ui/Dialog'

export function AccountChanged() {
  const t = useT()
  return (
    <AuthLayout>
      <Dialog
        open
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
