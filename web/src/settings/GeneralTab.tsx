// General: the workspace's own settings, for its admins: its default language, which members
// get when their browser asks for a language the app doesn't have, and whether the server can
// send email (docs/install.md#email), with a test message to the admin's own address.

import { type FormEvent, useEffect, useState } from 'react'
import { api, type MailStatus, mailPath, type Workspace } from '../api'
import { DEFAULT_LOCALE, LOCALES, localeName, setPreferences, useT } from '../i18n'
import { useLoad } from '../load'
import { Alert, useAction } from './parts'

// `operator`: they run the server, so the server's own email is theirs to see; other admins' routes for it
// answer 403.
export function GeneralTab({ workspace, email, operator }: { workspace: Workspace; email: string; operator: boolean }) {
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
      {operator && <MailPanel workspace={workspace.id} email={email} />}
    </div>
  )
}

// The server's outgoing email: where it sends through (never the account or its password), and
// a test message that goes to the admin's own address and nowhere else.
function MailPanel({ workspace, email }: { workspace: number; email: string }) {
  const t = useT()
  const status = useLoad(() => api<MailStatus>(mailPath(workspace)), [workspace])
  const [sentTo, setSentTo] = useState<string | null>(null)
  const { busy, error, run } = useAction()
  const send = async () => {
    setSentTo(null)
    await run(async () => {
      const r = await api<{ sent_to: string }>(`${mailPath(workspace)}/test`, { method: 'POST', body: {} })
      setSentTo(r.sent_to)
    })
  }
  const m = status.data
  return (
    <section className="tt-panel" aria-labelledby="tt-mail">
      <h2 id="tt-mail">{t('settings.general.mail.title')}</h2>
      <Alert error={status.error} />
      {!m && !status.error && <p className="tt-muted">{t('settings.general.mail.loading')}</p>}
      {m && !m.configured && <p className="tt-muted">{t('settings.general.mail.off')}</p>}
      {m?.configured && (
        <>
          <dl className="tt-endpoint">
            <dt>{t('settings.general.mail.server')}</dt>
            <dd>
              <code>{t('settings.general.mail.serverValue', { host: m.host, port: String(m.port) })}</code>
            </dd>
            <dt>{t('settings.general.mail.security')}</dt>
            <dd>{t(`settings.general.mail.securities.${m.security}`)}</dd>
            <dt>{t('settings.general.mail.from')}</dt>
            <dd>
              <code>{m.from}</code>
            </dd>
            <dt>{t('settings.general.mail.signIn')}</dt>
            <dd>
              {m.password === 'set' ? t('settings.general.mail.signInSet') : t('settings.general.mail.signInNone')}
            </dd>
          </dl>
          <div>
            <button type="button" className="tt-button" disabled={busy} onClick={send}>
              {busy ? t('settings.general.mail.testing') : t('settings.general.mail.test')}
            </button>{' '}
            <span className="tt-muted tt-small">{t('settings.general.mail.testHelp', { email })}</span>
          </div>
          <Alert error={error} />
          {sentTo && (
            <p role="status" className="tt-ok">
              {t('settings.general.mail.sent', { email: sentTo })}
            </p>
          )}
        </>
      )}
    </section>
  )
}
