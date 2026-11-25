// Another account's records that never reached the server are on this browser, and someone else
// has just signed in. Nothing of them is shown, and nothing else is either until the person
// chooses: sign in as that account to send them, or discard them (docs/offline.md).

import { useState } from 'react'
import { AuthLayout } from '../auth/AuthLayout'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { ConfirmDialog } from '../ui/ConfirmDialog'
import { Dialog } from '../ui/Dialog'

export function ForeignGuard({
  count,
  onDiscard,
  onSignOut,
}: {
  count: number
  onDiscard: () => void
  onSignOut: () => void
}) {
  const t = useT()
  const [discarding, setDiscarding] = useState(false)
  return (
    // Behind it, the logo only: nothing of either account.
    <AuthLayout>
      <Dialog
        open
        // Only an answer closes it: nothing else is shown until the person chooses.
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
            {/* The safe answer takes the focus: their records are kept for them. */}
            <Button variant="primary" onClick={onSignOut} data-autofocus>
              {t('offline.foreign.signOut')}
            </Button>
          </>
        }
      >
        <p>{t('offline.foreign.body', { count })}</p>
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
