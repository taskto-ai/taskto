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
