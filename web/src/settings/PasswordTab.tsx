// Your own password: the current one, then the new one twice. Changing it signs you out
// everywhere else; this browser gets a new session and stays signed in (docs/api.md#passwords).

import { type FormEvent, useState } from 'react'
import { api } from '../api'
import { useT } from '../i18n'
import { Alert, useAction } from './parts'

const empty = { current: '', next: '', again: '' }

export function PasswordTab() {
  const t = useT()
  const [form, setForm] = useState(empty)
  const [changed, setChanged] = useState(false)
  const action = useAction()
  const set = (key: keyof typeof form) => (e: { target: { value: string } }) => {
    setForm((f) => ({ ...f, [key]: e.target.value }))
    setChanged(false)
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
      setChanged(true)
    }
  }

  return (
    <div className="tt-settings">
      <section className="tt-panel" aria-labelledby="tt-password">
        <h2 id="tt-password">{t('settings.tabs.password')}</h2>
        <p className="tt-muted tt-small">{t('settings.password.intro')}</p>
        <form className="tt-form" onSubmit={submit}>
          <label>
            {t('settings.password.current')}
            <input
              type="password"
              value={form.current}
              onChange={set('current')}
              required
              autoComplete="current-password"
            />
          </label>
          <label>
            {t('settings.password.new')}
            <input
              type="password"
              value={form.next}
              onChange={set('next')}
              required
              minLength={12}
              autoComplete="new-password"
              aria-describedby="tt-new-password-hint"
            />
          </label>
          <span id="tt-new-password-hint" className="tt-hint">
            {t('settings.password.hint')}
          </span>
          <label>
            {t('settings.password.again')}
            <input
              type="password"
              value={form.again}
              onChange={set('again')}
              required
              autoComplete="new-password"
              aria-invalid={mismatch || undefined}
            />
          </label>
          <Alert error={mismatch ? t('settings.password.mismatch') : action.error} />
          <button type="submit" className="tt-button tt-primary" disabled={action.busy || mismatch}>
            {action.busy ? t('settings.password.changing') : t('settings.password.change')}
          </button>
        </form>
        {changed && (
          <p className="tt-notice" role="status">
            {t('settings.password.changed')}
          </p>
        )}
      </section>
    </div>
  )
}
