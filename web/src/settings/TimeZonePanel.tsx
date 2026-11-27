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

export function TimeZonePanel({ initial, onSaved }: { initial: string; onSaved?: () => void }) {
  const t = useT()
  const [zone, setZone] = useState(initial)
  const [saved, setSaved] = useState(false)
  const action = useAction()
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
