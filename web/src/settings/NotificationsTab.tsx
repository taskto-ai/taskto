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
import { routePath } from '../router'
import { Badge } from '../ui/Badge'
import { Button, ButtonLink } from '../ui/Button'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { EmailNotifications } from './EmailNotifications'
import { Alert, FormActions, Loading, Section, SettingsTab, useAction } from './parts'
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
  // Counts the zone's saves, to load the summary's choices again.
  const [zoneSaves, setZoneSaves] = useState(0)
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

  const badge =
    state === 'on' ? (
      <Badge tone="success" dot>
        {t('notifications.settings.onBadge')}
      </Badge>
    ) : state === 'denied' ? (
      <Badge tone="warning">{t('notifications.settings.blockedBadge')}</Badge>
    ) : state === null ? undefined : (
      <Badge tone="neutral">{t('notifications.settings.offBadge')}</Badge>
    )

  return (
    <SettingsTab
      title={t('settings.tabs.notifications')}
      help={t('notifications.settings.inbox')}
      actions={
        <ButtonLink href={routePath({ workspace, section: 'notifications' })} icon="bell">
          {t('notifications.settings.openInbox')}
        </ButtonLink>
      }
    >
      <Section
        title={t('notifications.settings.pushTitle')}
        help={t('notifications.settings.pushIntro')}
        actions={badge}
      >
        <Panel padded className="settings-panel-body">
          {state === null && <Loading lines={1} />}
          {state === 'unsupported' && <Notice tone="neutral">{t('notifications.settings.unsupported')}</Notice>}
          {state === 'ios-install' && <Notice tone="info">{t('notifications.settings.iosInstall')}</Notice>}
          {state === 'denied' && <Notice tone="warning">{t('notifications.settings.denied')}</Notice>}
          {state === 'off' && (
            <>
              {dismissed && <Notice tone="neutral">{t('notifications.settings.dismissed')}</Notice>}
              <p className="settings-text">{t('notifications.settings.off')}</p>
              <FormActions>
                <Button loading={action.busy} onClick={turnOn}>
                  {t('notifications.settings.turnOn')}
                </Button>
              </FormActions>
            </>
          )}
          {state === 'on' && (
            <>
              <p className="settings-text" role="status">
                {t('notifications.settings.on')}
              </p>
              <FormActions>
                <Button loading={action.busy} onClick={turnOff}>
                  {t('notifications.settings.turnOff')}
                </Button>
              </FormActions>
            </>
          )}
          <Alert error={action.error} />
          <p className="settings-muted">{t('notifications.settings.caveat')}</p>
        </Panel>
      </Section>
      {/* The summary follows the time zone until a zone is chosen for it: it loads its choices again
          when the zone changes, so what it shows (and sends back with a new hour) is the server's. */}
      <TimeZonePanel initial={timeZone} onSaved={() => setZoneSaves((n) => n + 1)} />
      <EmailNotifications key={zoneSaves} workspace={workspace} />
    </SettingsTab>
  )
}
