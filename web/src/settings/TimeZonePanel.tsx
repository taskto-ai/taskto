// The person's time zone: the one their account keeps, which the server times what it does for them
// by: card reminders at 08:00 on the due date (and the overdue notice at 08:00 the next day), and the
// daily email summary unless they chose another zone for it. Their own, in every workspace.

import { useMemo, useState } from 'react'
import { api, type Me } from '../api'
import { useT } from '../i18n'
import { browserTimeZone, timeZones } from '../timezone'
import { Alert, useAction } from './parts'

// `onSaved`: called when the account has the new zone, for what shows what follows it (the summary's).
export function TimeZonePanel({ initial, onSaved }: { initial: string; onSaved?: () => void }) {
  const t = useT()
  const [zone, setZone] = useState(initial)
  const [saved, setSaved] = useState(false)
  const action = useAction()
  const list = useMemo(() => timeZones(zone), [zone])
  const here = browserTimeZone()

  const save = async (next: string) => {
    setSaved(false)
    const ok = await action.run(async () => {
      const me = await api<Me>('/api/me', { method: 'PATCH', body: { time_zone: next } })
      setZone(me.user.time_zone ?? next)
      onSaved?.()
    })
    setSaved(ok)
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
