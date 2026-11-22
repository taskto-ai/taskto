// Settings → About → Install the app: what to do on this device to add TaskTo.AI to it. The
// browser's own prompt is offered when it gave one; otherwise the steps for the platform,
// since iOS has no prompt at all.

import { useState } from 'react'
import { type Key, useT } from '../i18n'
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
    <section className="tt-panel" aria-labelledby="tt-install">
      <h2 id="tt-install">{t('pwa.install.title')}</h2>
      {info.installed ? (
        <p>{t('pwa.install.installed')}</p>
      ) : !info.secure ? (
        <p className="tt-warning">{t('pwa.install.insecure')}</p>
      ) : (
        <>
          <p>{t('pwa.install.intro')}</p>
          {info.canPrompt && (
            <p>
              <button
                type="button"
                className="tt-button tt-primary"
                onClick={async () => setDeclined((await promptInstall()) === 'dismissed')}
              >
                {t('pwa.install.button')}
              </button>
            </p>
          )}
          {(!info.canPrompt || declined) && (
            <ol className="tt-steps">
              {STEPS[info.platform].map((key) => (
                <li key={key}>{t(key)}</li>
              ))}
            </ol>
          )}
          <p className="tt-muted tt-small">{t('pwa.install.offline')}</p>
        </>
      )}
    </section>
  )
}
