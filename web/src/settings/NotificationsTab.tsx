// Notifications: whether this device is told about new notifications when the app isn't open
// (Web Push). It is optional and off until the person turns it on here, with a click; the inbox
// works either way and is what is true. What this page says depends on what this browser can do,
// found by asking it, and is honest about what push is: a prompt that may arrive late or not at
// all (docs/pwa.md#notifications).

import { useCallback, useEffect, useState } from 'react'
import { useT } from '../i18n'
import {
  disablePush,
  enablePush,
  PushDenied,
  type PushState,
  PushUnsubscribeFailed,
  pushState,
  sendPushStrings,
} from '../notifications/push'
import { pushStrings } from '../notifications/strings'
import { useInstall } from '../pwa/install'
import { Link, routePath } from '../router'
import { EmailNotifications } from './EmailNotifications'
import { Alert, useAction } from './parts'
import { TimeZonePanel } from './TimeZonePanel'

// `timeZone`: the person's own, "" for none yet.
export function NotificationsTab({
  me,
  workspace,
  timeZone = '',
}: {
  me: number
  workspace: number
  timeZone?: string
}) {
  const t = useT()
  const install = useInstall()
  const [state, setState] = useState<PushState | null>(null)
  // The person closed the browser's prompt without answering: push stays off, and they are told so.
  const [dismissed, setDismissed] = useState(false)
  const action = useAction()

  const { platform, installed } = install
  const refresh = useCallback(() => pushState({ platform, installed }).then(setState), [platform, installed])
  useEffect(() => {
    void refresh()
  }, [refresh])

  const turnOn = async () => {
    setDismissed(false)
    await action.run(async () => {
      try {
        await enablePush(me)
        await sendPushStrings(pushStrings(t))
      } catch (e) {
        if (!(e instanceof PushDenied)) throw e
        setDismissed(Notification.permission === 'default')
      }
    })
    await refresh()
  }
  const turnOff = async () => {
    // A failure leaves push on, as it is, with the reason beside the button to try again.
    await action.run(async () => {
      try {
        await disablePush()
      } catch (e) {
        if (e instanceof PushUnsubscribeFailed) throw new Error(t('notifications.settings.turnOffFailed'))
        throw e
      }
    })
    await refresh()
  }

  return (
    <div className="tt-settings">
      <section className="tt-panel" aria-labelledby="tt-notifications">
        <h2 id="tt-notifications">{t('notifications.settings.title')}</h2>
        <p>
          {t('notifications.settings.inbox')}{' '}
          <Link href={routePath({ workspace, section: 'notifications' })}>{t('notifications.settings.openInbox')}</Link>
        </p>
      </section>
      <section className="tt-panel" aria-labelledby="tt-push">
        <h2 id="tt-push">{t('notifications.settings.pushTitle')}</h2>
        <p className="tt-muted tt-small">{t('notifications.settings.pushIntro')}</p>
        {state === null && <p className="tt-muted">{t('app.loading')}</p>}
        {state === 'unsupported' && <p>{t('notifications.settings.unsupported')}</p>}
        {state === 'ios-install' && <p>{t('notifications.settings.iosInstall')}</p>}
        {state === 'denied' && <p role="status">{t('notifications.settings.denied')}</p>}
        {state === 'off' && (
          <>
            {dismissed && <p role="status">{t('notifications.settings.dismissed')}</p>}
            <p>{t('notifications.settings.off')}</p>
            <button type="button" className="tt-button" disabled={action.busy} onClick={turnOn}>
              {t('notifications.settings.turnOn')}
            </button>
          </>
        )}
        {state === 'on' && (
          <>
            <p role="status" className="tt-ok">
              {t('notifications.settings.on')}
            </p>
            <button type="button" className="tt-button" disabled={action.busy} onClick={turnOff}>
              {t('notifications.settings.turnOff')}
            </button>
          </>
        )}
        <Alert error={action.error} />
        <p className="tt-muted tt-small">{t('notifications.settings.caveat')}</p>
      </section>
      <TimeZonePanel initial={timeZone} />
      <EmailNotifications workspace={workspace} />
    </div>
  )
}
