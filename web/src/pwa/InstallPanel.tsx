// Settings → About → Install the app: what to do on this device to add TaskTo.AI to it. The
// browser's own prompt is offered when it gave one; otherwise the steps for the platform,
// since iOS has no prompt at all.

import { useState } from 'react'
import { type Key, useT } from '../i18n'
import { FormActions, Section } from '../settings/parts'
import { Button } from '../ui/Button'
import { Notice } from '../ui/Notice'
import { promptInstall, useInstall } from './install'

const STEPS: Record<'ios' | 'android' | 'desktop', Key[]> = {
  ios: ['pwa.install.ios.share', 'pwa.install.ios.add', 'pwa.install.ios.confirm'],
  android: ['pwa.install.android.menu', 'pwa.install.android.confirm'],
  desktop: ['pwa.install.desktop.menu'],
}

export function InstallPanel() {
  const t = useT()
  const info = useInstall()
  const [declined, setDeclined] = useState(false)
  return (
    <Section title={t('pwa.install.title')}>
      {info.installed ? (
        <p className="settings-text">{t('pwa.install.installed')}</p>
      ) : !info.secure ? (
        <Notice tone="warning" live={false}>
          {t('pwa.install.insecure')}
        </Notice>
      ) : (
        <>
          <p className="settings-text">{t('pwa.install.intro')}</p>
          {info.canPrompt && (
            <FormActions>
              <Button variant="primary" onClick={async () => setDeclined((await promptInstall()) === 'dismissed')}>
                {t('pwa.install.button')}
              </Button>
            </FormActions>
          )}
          {(!info.canPrompt || declined) && (
            <ol className="settings-list">
              {STEPS[info.platform].map((key) => (
                <li key={key}>{t(key)}</li>
              ))}
            </ol>
          )}
          <p className="settings-muted">{t('pwa.install.offline')}</p>
        </>
      )}
    </Section>
  )
}
