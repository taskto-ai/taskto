// The person's time zone: the one their account keeps, which the server times what it does for them
// by: card reminders at 08:00 on the due date (and the overdue notice at 08:00 the next day), and the
// daily email summary unless they chose another zone for it. Their own, in every workspace.

import { useEffect, useMemo, useState } from 'react'
import { api, type Me } from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { browserTimeZone, timeZones } from '../timezone'
import { Button } from '../ui/Button'
import { Field } from '../ui/Field'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Select } from '../ui/Select'
import { Alert, FormActions, Section, useAction } from './parts'

// `onSaved`: called when the account has the new zone, for what shows what follows it (the summary's).
// `initial`: the zone the app had when the person signed in, shown until the account's own is read: the app
// doesn't hear of a zone saved here, so coming back to this page would show the old one.
export function TimeZonePanel({ initial, onSaved }: { initial: string; onSaved?: () => void }) {
  const t = useT()
  const [zone, setZone] = useState(initial)
  const [saved, setSaved] = useState(false)
  const action = useAction()
  // The zone shown is the last one the account confirmed: its answer to a save, else what it said when this
  // page opened. A save that succeeded is newer than that first answer, which may come after it and is then
  // ignored; one that failed changes nothing.
  const [picked, setPicked] = useState(false)
  const account = useLoad(() => api<Me>('/api/me'), [])
  useEffect(() => {
    if (account.data && !picked) setZone(account.data.user.time_zone ?? '')
  }, [account.data, picked])
  const list = useMemo(() => timeZones(zone), [zone])
  const here = browserTimeZone()

  const save = async (next: string) => {
    setSaved(false)
    const ok = await action.run(async () => {
      const me = await api<Me>('/api/me', { method: 'PATCH', body: { time_zone: next } })
      setZone(me.user.time_zone ?? next)
      setPicked(true)
      onSaved?.()
    })
    setSaved(ok)
  }

  return (
    <Section title={t('notifications.settings.timeZoneTitle')} help={t('notifications.settings.timeZoneIntro')}>
      <Panel padded className="settings-panel-body">
        <Field label={t('notifications.settings.timeZoneLabel')}>
          <Select value={zone} disabled={action.busy} onChange={(e) => void save(e.target.value)}>
            <option value="">{t('notifications.settings.timeZoneNone')}</option>
            {list.map((z) => (
              <option key={z} value={z}>
                {z}
              </option>
            ))}
          </Select>
        </Field>
        {here && here !== zone && (
          <FormActions>
            <Button disabled={action.busy} onClick={() => void save(here)}>
              {t('notifications.settings.timeZoneUseBrowser', { zone: here })}
            </Button>
          </FormActions>
        )}
        <Alert error={action.error} />
        {saved && <Notice tone="success">{t('notifications.settings.timeZoneSaved')}</Notice>}
      </Panel>
    </Section>
  )
}
