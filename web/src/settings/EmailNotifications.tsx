import { useMemo, useState } from 'react'
import { api } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import {
  EMAIL_KINDS,
  type EmailChoices,
  type EmailKind,
  type EmailMode,
  emailPath,
  emailVerificationPath,
} from '../notifications/api'
import { timeZones } from '../timezone'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { Field } from '../ui/Field'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Select } from '../ui/Select'
import { Alert, FormActions, Loading, Section, useAction } from './parts'

const HOURS = Array.from({ length: 24 }, (_, h) => h)

export function EmailNotifications({ workspace }: { workspace: number }) {
  const t = useT()
  const f = useFormat()
  const loaded = useLoad(() => api<EmailChoices>(emailPath(workspace)), [workspace])
  const [saved, setSaved] = useState<EmailChoices | null>(null)
  const action = useAction()
  const choices = saved ?? loaded.data
  const zoneList = useMemo(() => timeZones(choices?.digest.timezone ?? 'UTC'), [choices?.digest.timezone])

  const label = (kind: EmailKind) => t(`notifications.settings.email.${kind}`)

  const save = async (next: {
    kinds?: Partial<Record<EmailKind, EmailMode>>
    digest?: { hour: number; timezone: string }
  }) => {
    await action.run(async () => {
      setSaved(await api<EmailChoices>(emailPath(workspace), { method: 'PUT', body: next }))
    })
  }

  const setKind = (kind: EmailKind, mode: EmailMode) => {
    if (!choices) return
    void save({ kinds: { [kind]: mode } })
  }

  const [linkSent, setLinkSent] = useState(false)
  const sendLink = async () => {
    setLinkSent(false)
    const ok = await action.run(async () => {
      await api(emailVerificationPath(workspace), { method: 'POST', body: {} })
      setSaved(await api<EmailChoices>(emailPath(workspace)))
    })
    setLinkSent(ok)
  }

  const anyOn = choices ? EMAIL_KINDS.some((k) => choices.kinds[k] !== 'off') : false
  const usesDigest = choices ? EMAIL_KINDS.some((k) => choices.kinds[k] === 'digest') : false

  const address = choices?.address
  return (
    <Section title={t('notifications.settings.emailTitle')} help={t('notifications.settings.emailIntro')}>
      {!choices && !loaded.error && <Loading lines={2} />}
      <Alert error={loaded.error} />
      {choices && !choices.available && (
        <Notice tone="neutral">
          {t(anyOn ? 'notifications.settings.emailUnavailableOff' : 'notifications.settings.emailUnavailable')}
        </Notice>
      )}
      {choices && (choices.available || anyOn) && (
        <Panel padded className="settings-panel-body">
          <div className="settings-choices">
            {EMAIL_KINDS.map((kind) => (
              <Field key={kind} label={label(kind)}>
                <Select
                  value={choices.kinds[kind]}
                  disabled={action.busy}
                  onChange={(e) => setKind(kind, e.target.value as EmailMode)}
                >
                  <option value="off">{t('notifications.settings.emailOff')}</option>
                  <option value="instant" disabled={!choices.available}>
                    {t('notifications.settings.emailInstant')}
                  </option>
                  <option value="digest" disabled={!choices.available}>
                    {t('notifications.settings.emailDigest')}
                  </option>
                </Select>
              </Field>
            ))}
          </div>
          {choices.available && address && (
            <div className="settings-panel-body">
              <p className="settings-text settings-person-name">
                {t('notifications.settings.emailAddress', { email: address.email })}
                {address.state === 'verified' ? (
                  <Badge tone="success" dot>
                    {t('notifications.settings.emailConfirmedBadge')}
                  </Badge>
                ) : (
                  <Badge tone="warning">{t('notifications.settings.emailUnconfirmedBadge')}</Badge>
                )}
              </p>
              <p className="settings-muted">
                {address.state === 'verified' && t('notifications.settings.emailAddressVerified')}
                {address.state === 'pending' && t('notifications.settings.emailAddressPending')}
                {address.state === 'unverified' && t('notifications.settings.emailAddressUnverified')}
              </p>
              {address.state !== 'verified' && (
                <FormActions>
                  <Button loading={action.busy} onClick={() => void sendLink()}>
                    {address.state === 'pending'
                      ? t('notifications.settings.emailResendLink')
                      : t('notifications.settings.emailSendLink')}
                  </Button>
                </FormActions>
              )}
              {linkSent && <Notice tone="success">{t('notifications.settings.emailLinkSent')}</Notice>}
            </div>
          )}
          {usesDigest && choices.available && (
            <div className="settings-form-row">
              <Field label={t('notifications.settings.emailHour')}>
                <Select
                  value={choices.digest.hour}
                  disabled={action.busy}
                  onChange={(e) =>
                    void save({ digest: { hour: Number(e.target.value), timezone: choices.digest.timezone } })
                  }
                >
                  {HOURS.map((h) => (
                    <option key={h} value={h}>
                      {f.time(new Date(2000, 0, 1, h))}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field label={t('notifications.settings.emailZone')}>
                <Select
                  value={choices.digest.timezone}
                  disabled={action.busy}
                  onChange={(e) => void save({ digest: { hour: choices.digest.hour, timezone: e.target.value } })}
                >
                  {zoneList.map((z) => (
                    <option key={z} value={z}>
                      {z}
                    </option>
                  ))}
                </Select>
              </Field>
            </div>
          )}
          <Alert error={action.error} />
          <p className="settings-muted">{t('notifications.settings.emailCaveat')}</p>
        </Panel>
      )}
    </Section>
  )
}
