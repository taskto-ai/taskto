import { useCallback, useEffect, useState } from 'react'
import { useT } from '../i18n'
import {
  disablePush,
  enablePush,
  PushBusy,
  PushDenied,
  type PushState,
  PushUnsubscribeFailed,
  pushState,
  sendPushStrings,
} from '../notifications/push'
import { pushStrings } from '../notifications/strings'
import { useInstall } from '../pwa/install'
import { routePath } from '../router'
import { SessionEnded } from '../session'
import { Badge } from '../ui/Badge'
import { Button, ButtonLink } from '../ui/Button'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { EmailNotifications } from './EmailNotifications'
import { Alert, FormActions, Loading, Section, SettingsTab, useAction } from './parts'
import { TimeZonePanel } from './TimeZonePanel'

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
  const [zoneSaves, setZoneSaves] = useState(0)
  const install = useInstall()
  const [state, setState] = useState<PushState | null>(null)
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
        if (e instanceof SessionEnded) return
        if (e instanceof PushBusy) throw new Error(t('notifications.settings.busy'))
        if (!(e instanceof PushDenied)) throw e
        setDismissed(Notification.permission === 'default')
      }
    })
    await refresh()
  }
  const turnOff = async () => {
    await action.run(async () => {
      try {
        await disablePush()
      } catch (e) {
        if (e instanceof SessionEnded) return
        if (e instanceof PushUnsubscribeFailed) throw new Error(t('notifications.settings.turnOffFailed'))
        if (e instanceof PushBusy) throw new Error(t('notifications.settings.busy'))
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
      <TimeZonePanel initial={timeZone} onSaved={() => setZoneSaves((n) => n + 1)} />
      <EmailNotifications key={zoneSaves} workspace={workspace} />
    </SettingsTab>
  )
}
