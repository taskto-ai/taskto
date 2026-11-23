// Another account is signed in on this browser than the one this tab was showing (someone signed
// in, or out, in another tab). Nothing of the earlier account's is on screen or in memory any more:
// this says only that, and the page is reloaded to go on as whoever is signed in (docs/offline.md).

import { useEffect, useRef } from 'react'
import { useT } from '../i18n'

export function AccountChanged() {
  const t = useT()
  const first = useRef<HTMLButtonElement>(null)
  useEffect(() => first.current?.focus(), [])
  return (
    <main className="tt-center" id="main">
      <div className="tt-guard-box" role="alertdialog" aria-modal="true" aria-labelledby="tt-account-title">
        <h2 id="tt-account-title">{t('offline.accountChanged.title')}</h2>
        <p>{t('offline.accountChanged.body')}</p>
        <div className="tt-row-actions">
          <button ref={first} type="button" className="tt-button tt-primary" onClick={() => window.location.reload()}>
            {t('offline.accountChanged.reload')}
          </button>
        </div>
      </div>
    </main>
  )
}
