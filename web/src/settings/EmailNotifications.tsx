// Email notifications: which kinds of notification are also mailed to the person, as they happen or
// in one summary a day, in this workspace. Off until chosen, and only where the server can send mail.
// A mail says what happened and links back, never what a card or an app holds (docs/pwa.md#notifications).

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
import { Alert, useAction } from './parts'

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

  // The summary is at the hour of the person's own time zone (Time zone, above) until they choose
  // another for it here: the server says which, so nothing about it is sent with a kind.
  const setKind = (kind: EmailKind, mode: EmailMode) => {
    if (!choices) return
    void save({ kinds: { [kind]: mode } })
  }

  const [linkSent, setLinkSent] = useState(false)
  const sendLink = async () => {
    setLinkSent(false)
    const ok = await action.run(async () => {
      await api(emailVerificationPath(workspace), { method: 'POST', body: {} })
      // The state is "waiting for the link" now.
      setSaved(await api<EmailChoices>(emailPath(workspace)))
    })
    setLinkSent(ok)
  }

  const anyOn = choices ? EMAIL_KINDS.some((k) => choices.kinds[k] !== 'off') : false
  const usesDigest = choices ? EMAIL_KINDS.some((k) => choices.kinds[k] === 'digest') : false

  return (
    <section className="tt-panel" aria-labelledby="tt-email">
      <h2 id="tt-email">{t('notifications.settings.emailTitle')}</h2>
      <p className="tt-muted tt-small">{t('notifications.settings.emailIntro')}</p>
      {!choices && !loaded.error && <p className="tt-muted">{t('app.loading')}</p>}
      <Alert error={loaded.error} />
      {choices && !choices.available && (
        <p role="status">
          {t(anyOn ? 'notifications.settings.emailUnavailableOff' : 'notifications.settings.emailUnavailable')}
        </p>
      )}
      {choices && (choices.available || anyOn) && (
        <>
          <ul className="tt-list">
            {EMAIL_KINDS.map((kind) => (
              <li key={kind} className="tt-inline-field">
                <label>
                  {label(kind)}
                  <select
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
                  </select>
                </label>
              </li>
            ))}
          </ul>
          {choices.available && (
            <div className="tt-field">
              <p className="tt-small">
                {t('notifications.settings.emailAddress', { email: choices.address.email })}{' '}
                {choices.address.state === 'verified' && t('notifications.settings.emailAddressVerified')}
                {choices.address.state === 'pending' && t('notifications.settings.emailAddressPending')}
                {choices.address.state === 'unverified' && t('notifications.settings.emailAddressUnverified')}
              </p>
              {choices.address.state !== 'verified' && (
                <button type="button" className="tt-button" disabled={action.busy} onClick={() => void sendLink()}>
                  {choices.address.state === 'pending'
                    ? t('notifications.settings.emailResendLink')
                    : t('notifications.settings.emailSendLink')}
                </button>
              )}
              {linkSent && <p role="status">{t('notifications.settings.emailLinkSent')}</p>}
            </div>
          )}
          {usesDigest && choices.available && (
            <div className="tt-inline-field">
              <label>
                {t('notifications.settings.emailHour')}
                <select
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
                </select>
              </label>
              <label>
                {t('notifications.settings.emailZone')}
                <select
                  value={choices.digest.timezone}
                  disabled={action.busy}
                  onChange={(e) => void save({ digest: { hour: choices.digest.hour, timezone: e.target.value } })}
                >
                  {zoneList.map((z) => (
                    <option key={z} value={z}>
                      {z}
                    </option>
                  ))}
                </select>
              </label>
            </div>
          )}
          <Alert error={action.error} />
          <p className="tt-muted tt-small">{t('notifications.settings.emailCaveat')}</p>
        </>
      )}
    </section>
  )
}
