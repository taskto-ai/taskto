// Search: the workspace's embeddings model, which admins choose, and each space's consent
// to it, which only the space's owner or editors give. Consent is a privacy decision: it
// sends the space's Markdown documents, and its searches, from the server to one endpoint
// (account, model and base URL). So the endpoint is shown in full, consent names it back
// to the server (which refuses if it changed meanwhile), and a change of endpoint stops
// embedding until someone consents again.

import { type FormEvent, type ReactNode, useId, useState } from 'react'
import {
  api,
  type Consent,
  type ConsentEndpoint,
  type Member,
  type ProviderAccount,
  type SearchSettings,
  type Space,
  ws,
} from '../api'
import { useT } from '../i18n'
import { useLoad } from '../load'
import { spaceLabel } from '../spaces'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { Field } from '../ui/Field'
import { Heading } from '../ui/Heading'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Select } from '../ui/Select'
import { Alert, day, Facts, FormActions, Loading, Section, SettingsTab, useAction } from './parts'

export function SearchTab({ workspace, admin }: { workspace: number; admin: boolean }) {
  const base = ws(workspace)
  const settings = useLoad(() => api<SearchSettings>(`${base}/search/settings`), [base])
  const accounts = useLoad(
    () => api<{ providers: ProviderAccount[] }>(`${base}/providers`).then((r) => r.providers),
    [base],
  )
  const spaces = useLoad(() => api<{ spaces: Space[] }>(`${base}/spaces`).then((r) => r.spaces), [base])
  const members = useLoad(() => api<{ members: Member[] }>(`${base}/members`).then((r) => r.members), [base])
  // A new endpoint changes what every space's consent is for.
  const [round, setRound] = useState(0)
  const t = useT()

  return (
    <SettingsTab title={t('settings.tabs.search')} help={t('settings.help.search')}>
      <Section
        title={t('settings.search.title')}
        help={t('settings.search.intro')}
        actions={
          settings.data &&
          (settings.data.embeddings ? (
            <Badge tone="success" dot>
              {t('settings.search.onBadge')}
            </Badge>
          ) : (
            <Badge tone="neutral">{t('settings.search.offBadge')}</Badge>
          ))
        }
      >
        <Alert error={settings.error ?? accounts.error} />
        {!settings.data && !settings.error && <Loading lines={2} />}
        {settings.data && <Status settings={settings.data} accounts={accounts.data} />}
        {admin && settings.data && accounts.data && (
          <EmbeddingsForm
            // A new form for each saved endpoint: after a save, or another admin's change seen on
            // reload, it starts from what the server has, never from what this form held.
            key={
              settings.data.embeddings ? `${settings.data.embeddings.account}/${settings.data.embeddings.model}` : 'off'
            }
            base={base}
            settings={settings.data}
            accounts={accounts.data}
            onSaved={() => {
              settings.reload()
              setRound((r) => r + 1)
            }}
          />
        )}
      </Section>

      <Section title={t('settings.search.yourSpaces')} help={t('settings.search.spacesIntro')}>
        <Alert error={spaces.error} />
        {!spaces.data && !spaces.error && <Loading />}
        {spaces.data?.map((s) => (
          <SpaceConsent
            key={`${s.id}/${round}`}
            url={`${base}/spaces/${s.id}/search/embedding`}
            space={s}
            accounts={accounts.data}
            members={members.data}
          />
        ))}
      </Section>
    </SettingsTab>
  )
}

function accountName(accounts: ProviderAccount[] | undefined, id: number): string | undefined {
  return accounts?.find((a) => a.id === id)?.display_name
}

function Status({ settings, accounts }: { settings: SearchSettings; accounts: ProviderAccount[] | undefined }) {
  const t = useT()
  const e = settings.embeddings
  if (!e) return <Notice tone="neutral">{t('settings.search.noModel')}</Notice>
  return (
    <p className="settings-text">
      {t.rich('settings.search.status', {
        model: <code>{e.model}</code>,
        account: accountName(accounts, e.account) ?? t('settings.search.anAccount'),
        provider: <code>{e.provider}</code>,
      })}{' '}
      {settings.pending > 0
        ? t('settings.search.documentsPending', { count: settings.documents, pending: settings.pending })
        : t('settings.search.documents', { count: settings.documents })}
    </p>
  )
}

function EmbeddingsForm({
  base,
  settings,
  accounts,
  onSaved,
}: {
  base: string
  settings: SearchSettings
  accounts: ProviderAccount[]
  onSaved: () => void
}) {
  const [account, setAccount] = useState(String(settings.embeddings?.account ?? accounts[0]?.id ?? ''))
  const [model, setModel] = useState(settings.embeddings?.model ?? '')
  const action = useAction()
  const t = useT()
  const put = (embeddings: object | null) =>
    action
      .run(() => api(`${base}/search/settings`, { method: 'PUT', body: { embeddings } }))
      .then((ok) => ok && onSaved())
  const submit = (e: FormEvent) => {
    e.preventDefault()
    put({ account: Number(account), model: model.trim() })
  }
  const chosen = accounts.find((a) => String(a.id) === account)
  const listId = useId()

  if (accounts.length === 0) {
    return <Notice tone="neutral">{t('settings.search.accountsFirst')}</Notice>
  }
  return (
    <Panel padded>
      <form className="settings-form" onSubmit={submit} aria-label={t('settings.search.form')}>
        <div className="settings-form-row">
          <Field label={t('settings.search.account')}>
            <Select value={account} onChange={(e) => setAccount(e.target.value)}>
              {accounts.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.display_name}
                </option>
              ))}
            </Select>
          </Field>
          <Field
            label={t('settings.actions.model')}
            hint={chosen && t.rich('settings.search.servedAt', { url: <code>{`${chosen.base_url}/embeddings`}</code> })}
          >
            <Input
              required
              value={model}
              onChange={(e) => setModel(e.target.value)}
              maxLength={256}
              list={listId}
              placeholder="nomic-embed-text"
              spellCheck={false}
            />
          </Field>
          <datalist id={listId}>
            {chosen?.models.map((m) => (
              <option key={m.id} value={m.id} />
            ))}
          </datalist>
        </div>
        <Notice tone="warning">{t('settings.search.changeWarning')}</Notice>
        <Alert error={action.error} />
        <FormActions>
          <Button type="submit" variant="primary" loading={action.busy}>
            {t('settings.actions.save')}
          </Button>
          {settings.embeddings && (
            <Button variant="ghost" disabled={action.busy} onClick={() => put(null)}>
              {t('settings.search.turnOff')}
            </Button>
          )}
        </FormActions>
      </form>
    </Panel>
  )
}

function Endpoint({ endpoint, accounts }: { endpoint: ConsentEndpoint; accounts: ProviderAccount[] | undefined }) {
  const t = useT()
  const name = accountName(accounts, endpoint.account)
  return (
    <Facts
      items={[
        {
          term: t('settings.search.account'),
          value: (
            <>
              {name ? `${name} ` : ''}
              <code>{`#${endpoint.account}`}</code>
            </>
          ),
        },
        { term: t('settings.search.provider'), value: <code>{endpoint.provider}</code> },
        { term: t('settings.actions.model'), value: <code>{endpoint.model}</code> },
        { term: t('settings.actions.baseUrl'), value: <code>{endpoint.base_url}</code> },
      ]}
    />
  )
}

function SpaceConsent({
  url,
  space,
  accounts,
  members,
}: {
  url: string
  space: Space
  accounts: ProviderAccount[] | undefined
  members: Member[] | undefined
}) {
  const consent = useLoad(() => api<Consent>(url), [url])
  const [shown, setShown] = useState<Consent | null>(null)
  const [asking, setAsking] = useState(false)
  const action = useAction()
  const t = useT()
  const c = shown ?? consent.data
  const canDecide = space.role === 'owner' || space.role === 'editor'
  const label = spaceLabel(space)
  const by =
    c?.given_by != null ? (members?.find((m) => m.id === c.given_by)?.name ?? t('settings.search.formerMember')) : null
  const strong = (text: ReactNode) => <strong>{text}</strong>

  const put = async (body: object) => {
    const ok = await action.run(async () => setShown(await api<Consent>(url, { method: 'PUT', body })))
    setAsking(false)
    if (!ok) {
      // Refused, say because the endpoint changed meanwhile: show it as it is now, to review.
      setShown(null)
      consent.reload()
    }
  }

  return (
    <Panel as="section" padded className="settings-panel-body" aria-label={label}>
      <header className="settings-section-head">
        <div className="settings-section-titles">
          <Heading level={3}>{label}</Heading>
        </div>
        {c?.endpoint &&
          (c.enabled ? (
            <Badge tone="success" dot>
              {t('settings.search.consentOn')}
            </Badge>
          ) : c.given_at ? (
            <Badge tone="warning">{t('settings.search.consentPaused')}</Badge>
          ) : (
            <Badge tone="neutral">{t('settings.search.consentOff')}</Badge>
          ))}
      </header>
      <Alert error={consent.error} />
      {!c && !consent.error && <Loading lines={2} />}
      {c && !c.endpoint && <p className="settings-muted">{t('settings.search.fullTextOnly')}</p>}
      {c?.endpoint && (
        <>
          <p className="settings-text">
            {c.enabled
              ? by && c.given_at
                ? t.rich('settings.search.onBy', { name: by, date: day(c.given_at), strong })
                : t.rich('settings.search.on', { strong })
              : c.given_at
                ? t.rich('settings.search.paused', { strong })
                : t.rich('settings.search.offState', { strong })}
          </p>
          <Endpoint endpoint={c.endpoint} accounts={accounts} />
          {!canDecide && (
            <p className="settings-muted">
              {space.kind === 'private' ? t('settings.search.onlyOwner') : t('settings.search.onlyEditors')}
            </p>
          )}
          {canDecide && c.enabled && (
            <FormActions>
              <Button disabled={action.busy} onClick={() => put({ enabled: false })}>
                {t('settings.search.withdraw')}
              </Button>
              <span className="settings-muted">{t('settings.search.withdrawNote')}</span>
            </FormActions>
          )}
          {canDecide && !c.enabled && !asking && (
            <FormActions>
              <Button disabled={action.busy} onClick={() => setAsking(true)}>
                {t('settings.search.consent')}
              </Button>
            </FormActions>
          )}
          {canDecide && !c.enabled && asking && (
            <fieldset className="settings-fieldset" aria-label={t('settings.search.consentFor', { name: label })}>
              <Notice tone="warning" title={t('settings.search.consentFor', { name: label })}>
                {t.rich(space.kind === 'private' ? 'settings.search.askPrivate' : 'settings.search.askShared', {
                  name: label,
                  url: <code>{c.endpoint.base_url}</code>,
                  model: <code>{c.endpoint.model}</code>,
                })}
              </Notice>
              <FormActions>
                <Button disabled={action.busy} onClick={() => put({ enabled: true, endpoint: c.endpoint })}>
                  {t('settings.search.yes')}
                </Button>
                <Button variant="ghost" onClick={() => setAsking(false)}>
                  {t('common.cancel')}
                </Button>
              </FormActions>
            </fieldset>
          )}
        </>
      )}
      <Alert error={action.error} />
    </Panel>
  )
}
