// Another account's records that never reached the server are on this browser, and someone else
// has just signed in. Nothing of them is shown, and nothing else is either until the person
// chooses: sign in as that account to send them, or discard them (docs/offline.md).

import { useEffect, useRef } from 'react'
import { useT } from '../i18n'
import { ConfirmButton } from '../settings/parts'

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
  const first = useRef<HTMLButtonElement>(null)
  useEffect(() => first.current?.focus(), [])
  return (
    <main className="tt-center" id="main">
      <div className="tt-guard-box" role="alertdialog" aria-modal="true" aria-labelledby="tt-foreign-title">
        <h2 id="tt-foreign-title">{t('offline.foreign.title')}</h2>
        <p>{t('offline.foreign.body', { count })}</p>
        <div className="tt-row-actions">
          <button ref={first} type="button" className="tt-button tt-primary" onClick={onSignOut}>
            {t('offline.foreign.signOut')}
          </button>
          <ConfirmButton
            label={t('offline.foreign.discard')}
            question={t('offline.foreign.discardQuestion', { count })}
            confirm={t('offline.queue.discardConfirm')}
            onConfirm={onDiscard}
          />
        </div>
      </div>
    </main>
  )
}
