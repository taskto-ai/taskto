// "A new version is ready": shown when the service worker has installed an update. The person
// updates when they choose; the page is never reloaded for them (src/pwa/update.ts). It looks like
// a toast, at the window's bottom, but unlike one it stays until it is answered.

import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Icon } from '../ui/Icon'
import { applyUpdate, dismissUpdate, useUpdatePrompt } from './update'
import './UpdateBanner.css'

export function UpdateBanner() {
  const t = useT()
  if (!useUpdatePrompt()) return null
  return (
    <section className="ui-update" role="status">
      <span className="ui-update-icon" aria-hidden="true">
        <Icon name="info" />
      </span>
      <p className="ui-update-message">{t('pwa.update.message')}</p>
      <div className="ui-update-actions">
        <Button size="sm" variant="ghost" onClick={dismissUpdate}>
          {t('pwa.update.later')}
        </Button>
        <Button size="sm" variant="primary" onClick={applyUpdate}>
          {t('pwa.update.reload')}
        </Button>
      </div>
    </section>
  )
}
