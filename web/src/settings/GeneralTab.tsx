// General: the workspace's own settings, for its admins: its default language, which members
// get when their browser asks for a language the app doesn't have, and whether the server can
// send email (docs/install.md#email), with a test message to the admin's own address.

import { type FormEvent, useEffect, useState } from 'react'
import { api, type MailStatus, mailPath, type Workspace } from '../api'
import { DEFAULT_LOCALE, LOCALES, localeName, setPreferences, t as translate, useT } from '../i18n'
import { useLoad } from '../load'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { Field } from '../ui/Field'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Select } from '../ui/Select'
import { toast } from '../ui/Toast'
import { Alert, Facts, FormActions, Loading, Section, SettingsTab, useAction } from './parts'

// `operator`: they run the server, so the server's own email is theirs to see; other admins' routes for it
// answer 403.
export function GeneralTab({ workspace, email, operator }: { workspace: Workspace; email: string; operator: boolean }) {
  const t = useT()
  const [locale, setLocale] = useState(workspace.locale || DEFAULT_LOCALE)
  // The workspace's value from the server, when it comes again.
  useEffect(() => setLocale(workspace.locale || DEFAULT_LOCALE), [workspace.locale])
  const { busy, error, run } = useAction()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    // English, the default, is stored as "": no default picked.
    const body = { locale: locale === DEFAULT_LOCALE ? '' : locale }
    const ok = await run(() => api(`/api/workspaces/${workspace.id}`, { method: 'PATCH', body }))
    if (ok) {
      await setPreferences({ workspace: body.locale })
      // In the language the page is in now, which the change may have made another.
      toast({ message: translate('settings.general.saved'), tone: 'success' })
    }
  }
  return (
    <SettingsTab title={t('settings.tabs.general')} help={t('settings.help.general')}>
      <Panel padded>
        <form className="settings-form" onSubmit={submit}>
          <Field label={t('settings.general.language')} hint={t('settings.general.languageHelp')}>
            <Select value={locale} onChange={(e) => setLocale(e.target.value)}>
              {LOCALES.map((tag) => (
                <option key={tag} value={tag} lang={tag}>
                  {localeName(tag)}
                </option>
              ))}
            </Select>
          </Field>
          <Alert error={error} />
          <FormActions>
            <Button type="submit" variant="primary" loading={busy}>
              {t('settings.general.save')}
            </Button>
          </FormActions>
        </form>
      </Panel>
      {operator && <MailPanel workspace={workspace.id} email={email} />}
    </SettingsTab>
  )
}

// The server's outgoing email: where it sends through (never the account or its password), and
// a test message that goes to the admin's own address and nowhere else.
function MailPanel({ workspace, email }: { workspace: number; email: string }) {
  const t = useT()
  const status = useLoad(() => api<MailStatus>(mailPath(workspace)), [workspace])
  const { busy, error, run } = useAction()
  const send = async () => {
    let to = ''
    const ok = await run(async () => {
      to = (await api<{ sent_to: string }>(`${mailPath(workspace)}/test`, { method: 'POST', body: {} })).sent_to
    })
    if (ok) toast({ message: t('settings.general.mail.sent', { email: to }), tone: 'success' })
  }
  const m = status.data
  return (
    <Section
      title={t('settings.general.mail.title')}
      actions={
        m &&
        (m.configured ? (
          <Badge tone="success" dot>
            {t('settings.general.mail.on')}
          </Badge>
        ) : (
          <Badge tone="neutral">{t('settings.general.mail.offBadge')}</Badge>
        ))
      }
    >
      <Alert error={status.error} />
      {!m && !status.error && <Loading lines={2} />}
      {m && !m.configured && <Notice tone="neutral">{t('settings.general.mail.off')}</Notice>}
      {m?.configured && (
        <Panel padded className="settings-panel-body">
          <Facts
            items={[
              {
                term: t('settings.general.mail.server'),
                value: <code>{t('settings.general.mail.serverValue', { host: m.host, port: String(m.port) })}</code>,
              },
              { term: t('settings.general.mail.security'), value: t(`settings.general.mail.securities.${m.security}`) },
              { term: t('settings.general.mail.from'), value: <code>{m.from}</code> },
              {
                term: t('settings.general.mail.signIn'),
                value:
                  m.password === 'set' ? t('settings.general.mail.signInSet') : t('settings.general.mail.signInNone'),
              },
            ]}
          />
          {m.sends_links === false && <Notice tone="neutral">{t('settings.general.mail.noLinks')}</Notice>}
          <FormActions>
            <Button loading={busy} onClick={send}>
              {t('settings.general.mail.test')}
            </Button>
            <span className="settings-muted">{t('settings.general.mail.testHelp', { email })}</span>
          </FormActions>
          <Alert error={error} />
        </Panel>
      )}
    </Section>
  )
}
