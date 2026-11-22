// Search: the workspace's embeddings model, which admins choose, and each space's consent
// to it, which only the space's owner or editors give. Consent is a privacy decision: it
// sends the space's Markdown documents, and its searches, from the server to one endpoint
// (account, model and base URL). So the endpoint is shown in full, consent names it back
// to the server (which refuses if it changed meanwhile), and a change of endpoint stops
// embedding until someone consents again.

import { type FormEvent, type ReactNode, useState } from 'react'
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
import { Alert, day, useAction } from './parts'

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
    <div className="tt-settings">
      <section className="tt-panel" aria-labelledby="tt-embeddings">
        <h2 id="tt-embeddings">{t('settings.search.title')}</h2>
        <p className="tt-muted tt-small">{t('settings.search.intro')}</p>
        <Alert error={settings.error ?? accounts.error} />
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
      </section>

      <section className="tt-panel" aria-labelledby="tt-consent">
        <h2 id="tt-consent">{t('settings.search.yourSpaces')}</h2>
        <p className="tt-muted tt-small">{t('settings.search.spacesIntro')}</p>
        <Alert error={spaces.error} />
        {!spaces.data && !spaces.error && <p className="tt-muted">{t('app.loading')}</p>}
        {spaces.data?.map((s) => (
          <SpaceConsent
            key={`${s.id}/${round}`}
            url={`${base}/spaces/${s.id}/search/embedding`}
            space={s}
            accounts={accounts.data}
            members={members.data}
          />
        ))}
      </section>
    </div>
  )
}

function accountName(accounts: ProviderAccount[] | undefined, id: number): string | undefined {
  return accounts?.find((a) => a.id === id)?.display_name
}

function Status({ settings, accounts }: { settings: SearchSettings; accounts: ProviderAccount[] | undefined }) {
  const t = useT()
  const e = settings.embeddings
  if (!e) return <p>{t('settings.search.noModel')}</p>
  return (
    <p>
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

  if (accounts.length === 0) {
    return <p className="tt-muted">{t('settings.search.accountsFirst')}</p>
  }
  return (
    <form className="tt-form" onSubmit={submit} aria-label={t('settings.search.form')}>
      <div className="tt-form-row">
        <label>
          {t('settings.search.account')}
          <select value={account} onChange={(e) => setAccount(e.target.value)}>
            {accounts.map((a) => (
              <option key={a.id} value={a.id}>
                {a.display_name}
              </option>
            ))}
          </select>
        </label>
        <label>
          {t('settings.actions.model')}
          <input
            value={model}
            onChange={(e) => setModel(e.target.value)}
            required
            maxLength={256}
            list="tt-embed-models"
            placeholder="nomic-embed-text"
            spellCheck={false}
          />
          <datalist id="tt-embed-models">
            {chosen?.models.map((m) => (
              <option key={m.id} value={m.id} />
            ))}
          </datalist>
        </label>
      </div>
      {chosen && (
        <p className="tt-muted tt-small">
          {t.rich('settings.search.servedAt', { url: <code>{`${chosen.base_url}/embeddings`}</code> })}
        </p>
      )}
      <p className="tt-warning tt-small">{t('settings.search.changeWarning')}</p>
      <Alert error={action.error} />
      <div className="tt-row-actions">
        <button type="submit" className="tt-button tt-primary" disabled={action.busy}>
          {action.busy ? t('settings.search.checking') : t('settings.actions.save')}
        </button>
        {settings.embeddings && (
          <button type="button" className="tt-button" disabled={action.busy} onClick={() => put(null)}>
            {t('settings.search.turnOff')}
          </button>
        )}
      </div>
    </form>
  )
}

function Endpoint({ endpoint, accounts }: { endpoint: ConsentEndpoint; accounts: ProviderAccount[] | undefined }) {
  const t = useT()
  const name = accountName(accounts, endpoint.account)
  return (
    <dl className="tt-endpoint">
      <dt>{t('settings.search.account')}</dt>
      <dd>
        {name ? `${name} ` : ''}
        <code>{`#${endpoint.account}`}</code>
      </dd>
      <dt>{t('settings.search.provider')}</dt>
      <dd>
        <code>{endpoint.provider}</code>
      </dd>
      <dt>{t('settings.actions.model')}</dt>
      <dd>
        <code>{endpoint.model}</code>
      </dd>
      <dt>{t('settings.actions.baseUrl')}</dt>
      <dd>
        <code>{endpoint.base_url}</code>
      </dd>
    </dl>
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
    <article className="tt-consent" aria-label={label} data-enabled={c?.enabled || undefined}>
      <h3>{label}</h3>
      <Alert error={consent.error} />
      {!c && !consent.error && <p className="tt-muted">{t('app.loading')}</p>}
      {c && !c.endpoint && <p>{t('settings.search.fullTextOnly')}</p>}
      {c?.endpoint && (
        <>
          {c.enabled ? (
            <p>
              {by && c.given_at
                ? t.rich('settings.search.onBy', { name: by, date: day(c.given_at), strong })
                : t.rich('settings.search.on', { strong })}
            </p>
          ) : c.given_at ? (
            <p>{t.rich('settings.search.paused', { strong })}</p>
          ) : (
            <p>{t.rich('settings.search.offState', { strong })}</p>
          )}
          <Endpoint endpoint={c.endpoint} accounts={accounts} />
          {!canDecide && (
            <p className="tt-muted tt-small">
              {space.kind === 'private' ? t('settings.search.onlyOwner') : t('settings.search.onlyEditors')}
            </p>
          )}
          {canDecide && c.enabled && (
            <div className="tt-row-actions">
              <button
                type="button"
                className="tt-button"
                disabled={action.busy}
                onClick={() => put({ enabled: false })}
              >
                {t('settings.search.withdraw')}
              </button>
              <span className="tt-muted tt-small">{t('settings.search.withdrawNote')}</span>
            </div>
          )}
          {canDecide && !c.enabled && !asking && (
            <div className="tt-row-actions">
              <button type="button" className="tt-button" disabled={action.busy} onClick={() => setAsking(true)}>
                {t('settings.search.consent')}
              </button>
            </div>
          )}
          {canDecide && !c.enabled && asking && (
            <fieldset className="tt-confirm-box" aria-label={t('settings.search.consentFor', { name: label })}>
              <p>
                {t.rich(space.kind === 'private' ? 'settings.search.askPrivate' : 'settings.search.askShared', {
                  name: label,
                  url: <code>{c.endpoint.base_url}</code>,
                  model: <code>{c.endpoint.model}</code>,
                })}
              </p>
              <div className="tt-row-actions">
                <button
                  type="button"
                  className="tt-button tt-primary"
                  disabled={action.busy}
                  onClick={() => put({ enabled: true, endpoint: c.endpoint })}
                >
                  {t('settings.search.yes')}
                </button>
                <button type="button" className="tt-button" onClick={() => setAsking(false)}>
                  {t('common.cancel')}
                </button>
              </div>
            </fieldset>
          )}
        </>
      )}
      <Alert error={action.error} />
    </article>
  )
}
