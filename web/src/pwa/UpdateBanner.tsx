// "A new version is ready": shown when the service worker has installed an update. The person
// updates when they choose; the page is never reloaded for them (src/pwa/update.ts).

import { useT } from '../i18n'
import { applyUpdate, dismissUpdate, useUpdatePrompt } from './update'

export function UpdateBanner() {
  const t = useT()
  if (!useUpdatePrompt()) return null
  return (
    <div className="tt-update" role="status">
      <p>{t('pwa.update.message')}</p>
      <div className="tt-update-actions">
        <button type="button" className="tt-button tt-primary" onClick={applyUpdate}>
          {t('pwa.update.reload')}
        </button>
        <button type="button" className="tt-button" onClick={dismissUpdate}>
          {t('pwa.update.later')}
        </button>
      </div>
    </div>
  )
}
