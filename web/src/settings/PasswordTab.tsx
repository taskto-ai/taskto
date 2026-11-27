import { type FormEvent, useState } from 'react'
import { api } from '../api'
import { useT } from '../i18n'
import { Button } from '../ui/Button'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Panel } from '../ui/Panel'
import { toast } from '../ui/Toast'
import { Alert, FormActions, Section, SettingsTab, useAction } from './parts'
import { TwoFactorPanel } from './TwoFactorPanel'

const empty = { current: '', next: '', again: '' }

export function PasswordTab() {
  const t = useT()
  const [form, setForm] = useState(empty)
  const action = useAction()
  const set = (key: keyof typeof form) => (e: { target: { value: string } }) => {
    setForm((f) => ({ ...f, [key]: e.target.value }))
  }
  const mismatch = form.again !== '' && form.next !== form.again

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (form.next !== form.again) return
    const ok = await action.run(() =>
      api('/api/me/password', {
        method: 'POST',
        body: { current_password: form.current, new_password: form.next },
      }),
    )
    if (ok) {
      setForm(empty)
      toast({ message: t('settings.password.changed'), tone: 'success' })
    }
  }

  return (
    <SettingsTab title={t('settings.tabs.password')} help={t('settings.help.password')}>
      <Section title={t('settings.password.title')} help={t('settings.password.intro')}>
        <Panel padded>
          <form className="settings-form" onSubmit={submit}>
            <Field label={t('settings.password.current')} required>
              <Input type="password" value={form.current} onChange={set('current')} autoComplete="current-password" />
            </Field>
            <Field label={t('settings.password.new')} hint={t('settings.password.hint')} required>
              <Input
                type="password"
                value={form.next}
                onChange={set('next')}
                minLength={12}
                autoComplete="new-password"
              />
            </Field>
            <Field
              label={t('settings.password.again')}
              error={mismatch ? t('settings.password.mismatch') : undefined}
              required
            >
              <Input type="password" value={form.again} onChange={set('again')} autoComplete="new-password" />
            </Field>
            <Alert error={action.error} />
            <FormActions>
              <Button type="submit" variant="primary" loading={action.busy} disabled={mismatch}>
                {t('settings.password.change')}
              </Button>
            </FormActions>
          </form>
        </Panel>
      </Section>
      <TwoFactorPanel />
    </SettingsTab>
  )
}
