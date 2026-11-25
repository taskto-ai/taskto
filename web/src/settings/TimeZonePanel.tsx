// The person's time zone: the one their account keeps, which the server times what it does for them
// by: card reminders at 08:00 on the due date (and the overdue notice at 08:00 the next day), and the
// daily email summary unless they chose another zone for it. Their own, in every workspace.

import { useEffect, useMemo, useState } from 'react'
import { api, type Me } from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { browserTimeZone, timeZones } from '../timezone'
import { Alert, useAction } from './parts'

// `onSaved`: called when the account has the new zone, for what shows what follows it (the summary's).
// `initial`: the zone the app had when the person signed in, shown until the account's own is read: the app
// doesn't hear of a zone saved here, so coming back to this page would show the old one.
export function TimeZonePanel({ initial, onSaved }: { initial: string; onSaved?: () => void }) {
  const t = useT()
  const [zone, setZone] = useState(initial)
  const [saved, setSaved] = useState(false)
  const action = useAction()
  // A pick that was saved is newer than what the account said before it: that answer is ignored. One
  // that failed is nothing, and the account's answer is the zone again.
  const [picked, setPicked] = useState(false)
  const account = useLoad(() => api<Me>('/api/me'), [])
  useEffect(() => {
    if (account.data && !picked) setZone(account.data.user.time_zone ?? '')
  }, [account.data, picked])
  const list = useMemo(() => timeZones(zone), [zone])
  const here = browserTimeZone()

  const save = async (next: string) => {
    setSaved(false)
    setPicked(true)
    const ok = await action.run(async () => {
      const me = await api<Me>('/api/me', { method: 'PATCH', body: { time_zone: next } })
      setZone(me.user.time_zone ?? next)
      onSaved?.()
    })
    setSaved(ok)
    if (!ok) setPicked(false)
  }

  return (
    <section className="tt-panel" aria-labelledby="tt-time-zone">
      <h2 id="tt-time-zone">{t('notifications.settings.timeZoneTitle')}</h2>
      <p className="tt-muted tt-small">{t('notifications.settings.timeZoneIntro')}</p>
      <div className="tt-inline-field">
        <label>
          {t('notifications.settings.timeZoneLabel')}
          <select value={zone} disabled={action.busy} onChange={(e) => void save(e.target.value)}>
            <option value="">{t('notifications.settings.timeZoneNone')}</option>
            {list.map((z) => (
              <option key={z} value={z}>
                {z}
              </option>
            ))}
          </select>
        </label>
        {here && here !== zone && (
          <button type="button" className="tt-button" disabled={action.busy} onClick={() => void save(here)}>
            {t('notifications.settings.timeZoneUseBrowser', { zone: here })}
          </button>
        )}
      </div>
      <Alert error={action.error} />
      {saved && <p role="status">{t('notifications.settings.timeZoneSaved')}</p>}
    </section>
  )
}
