// General: the workspace's own settings, for its admins. For now its default language, which
// members get when their browser asks for a language the app doesn't have.

import { type FormEvent, useEffect, useState } from 'react'
import { api, type Workspace } from '../api'
import { DEFAULT_LOCALE, LOCALES, localeName, setPreferences, useT } from '../i18n'
import { Alert, useAction } from './parts'

export function GeneralTab({ workspace }: { workspace: Workspace }) {
  const t = useT()
  const [locale, setLocale] = useState(workspace.locale || DEFAULT_LOCALE)
  // The workspace's value from the server, when it comes again.
  useEffect(() => setLocale(workspace.locale || DEFAULT_LOCALE), [workspace.locale])
  const [saved, setSaved] = useState(false)
  const { busy, error, run } = useAction()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setSaved(false)
    // English, the default, is stored as "": no default picked.
    const body = { locale: locale === DEFAULT_LOCALE ? '' : locale }
    const ok = await run(() => api(`/api/workspaces/${workspace.id}`, { method: 'PATCH', body }))
    if (ok) {
      setSaved(true)
      await setPreferences({ workspace: body.locale })
    }
  }
  return (
    <div className="tt-settings">
      <section className="tt-panel" aria-labelledby="tt-general">
        <h2 id="tt-general">{t('settings.tabs.general')}</h2>
        <form className="tt-form" onSubmit={submit}>
          <label>
            {t('settings.general.language')}
            <select
              value={locale}
              onChange={(e) => {
                setLocale(e.target.value)
                setSaved(false)
              }}
              aria-describedby="tt-general-language"
            >
              {LOCALES.map((tag) => (
                <option key={tag} value={tag} lang={tag}>
                  {localeName(tag)}
                </option>
              ))}
            </select>
          </label>
          <span id="tt-general-language" className="tt-muted tt-small">
            {t('settings.general.languageHelp')}
          </span>
          <Alert error={error} />
          <div>
            <button type="submit" className="tt-button tt-primary" disabled={busy}>
              {t('settings.general.save')}
            </button>
            {saved && (
              <span role="status" className="tt-muted">
                {' '}
                {t('settings.general.saved')}
              </span>
            )}
          </div>
        </form>
      </section>
    </div>
  )
}
