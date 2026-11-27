import { useState } from 'react'
import { AuthLayout } from '../auth/AuthLayout'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { ConfirmDialog } from '../ui/ConfirmDialog'
import { Dialog } from '../ui/Dialog'

export function ForeignGuard({
  count,
  failed = false,
  onDiscard,
  onSignOut,
}: {
  count: number
  failed?: boolean
  onDiscard: () => void
  onSignOut: () => void
}) {
  const t = useT()
  const [discarding, setDiscarding] = useState(false)
  return (
    <AuthLayout>
      <Dialog
        open
        dismissible={false}
        onClose={() => undefined}
        role="alertdialog"
        size="sm"
        title={t('offline.foreign.title')}
        actions={
          <>
            <Button variant="danger" onClick={() => setDiscarding(true)}>
              {t('offline.foreign.discard')}
            </Button>
            <Button variant="primary" onClick={onSignOut} data-autofocus>
              {t('offline.foreign.signOut')}
            </Button>
          </>
        }
      >
        <p>{t('offline.foreign.body', { count })}</p>
        {failed && <p role="alert">{t('offline.foreign.failed')}</p>}
      </Dialog>
      <ConfirmDialog
        open={discarding}
        title={t('offline.foreign.discard')}
        confirmLabel={t('offline.queue.discardConfirm')}
        tone="danger"
        onConfirm={() => {
          setDiscarding(false)
          onDiscard()
        }}
        onCancel={() => setDiscarding(false)}
      >
        {t('offline.foreign.discardQuestion', { count })}
      </ConfirmDialog>
    </AuthLayout>
  )
}
