// A workspace's shared CLI accounts (docs/account-switching.md): API keys its admins add for
// Claude Code, Codex or Grok Build, which members' turns use through the server's proxy. The key
// never reaches the CLIs, and never comes back from the server. They are never subscription
// logins: the providers' terms forbid sharing one. Admins also set which accounts each member's
// turns use here (their own, the workspace's, or their own if connected, else the workspace's).

import { type FormEvent, useState } from 'react'
import {
  api,
  CLI_PRODUCTS,
  type CliAccountMode,
  type CliMemberModes,
  type CliName,
  codeText,
  type SharedCliAccount,
  type SharedCliAccounts,
  ws,
} from '../api'
import { type Key, type Params, useT } from '../i18n'
import { useLoad } from '../load'
import { CliLimits } from './CliAccountsTab'
import { Alert, ConfirmButton, useAction } from './parts'

// The terms that govern an API key of each provider, linked next to the shared accounts.
export const SHARED_TERMS: Record<CliName, { label: Key; href: string }[]> = {
  claude: [
    { label: 'settings.sharedCli.terms.anthropicCommercial', href: 'https://www.anthropic.com/legal/commercial-terms' },
    { label: 'settings.sharedCli.terms.claudeCodeLegal', href: 'https://code.claude.com/docs/en/legal-and-compliance' },
  ],
  codex: [
    { label: 'settings.sharedCli.terms.openaiServices', href: 'https://openai.com/policies/services-agreement/' },
  ],
  grok: [{ label: 'settings.sharedCli.terms.xaiEnterprise', href: 'https://x.ai/legal/terms-of-service-enterprise' }],
}

export const MODE_LABEL: Record<CliAccountMode, Key> = {
  own_else_shared: 'settings.sharedCli.modes.own_else_shared',
  own: 'settings.sharedCli.modes.own',
  shared: 'settings.sharedCli.modes.shared',
}

// What the viewer's own mode means for them.
const YOURS: Record<CliAccountMode, Key> = {
  own_else_shared: 'settings.sharedCli.yours.own_else_shared',
  own: 'settings.sharedCli.yours.own',
  shared: 'settings.sharedCli.yours.shared',
}

const DEFAULT_URL: Record<CliName, string> = {
  claude: 'https://api.anthropic.com',
  codex: 'https://api.openai.com',
  grok: 'https://api.x.ai',
}

// A test's outcome: `code` and `params` are a failure's, for codeText().
type Test = { ok: boolean; message: string; code?: string; params?: Params }

export function SharedCliAccountsPanel({ workspace, admin }: { workspace: number; admin: boolean }) {
  const base = `${ws(workspace)}/cli-accounts`
  const list = useLoad(() => api<SharedCliAccounts>(base), [base])
  const [editing, setEditing] = useState<number | 'new' | null>(null)
  const t = useT()
  const done = () => {
    setEditing(null)
    list.reload()
  }
  return (
    <section className="tt-panel" aria-label={t('settings.sharedCli.title')}>
      <div className="tt-pane-head">
        <h2>{t('settings.sharedCli.title')}</h2>
        {admin && editing !== 'new' && (
          <button type="button" className="tt-button" onClick={() => setEditing('new')}>
            {t('settings.sharedCli.add')}
          </button>
        )}
      </div>
      <p className="tt-muted tt-small">{t('settings.sharedCli.intro')}</p>
      <ul className="tt-terms tt-small" aria-label={t('settings.sharedCli.termsLabel')}>
        {(Object.keys(SHARED_TERMS) as CliName[]).map((cli) => (
          <li key={cli}>
            {t('settings.sharedCli.termsOf', { name: CLI_PRODUCTS[cli] })}{' '}
            {SHARED_TERMS[cli].map((term, i) => (
              <span key={term.href}>
                {i > 0 && ' · '}
                <a href={term.href} target="_blank" rel="noopener noreferrer">
                  {t(term.label)}
                </a>
              </span>
            ))}
          </li>
        ))}
      </ul>
      <Alert error={list.error} />
      {list.data && !list.data.enabled && list.data.reason && (
        <p className="tt-notice">{codeText(list.data.reason_code, undefined, list.data.reason)}</p>
      )}
      {list.data && <p className="tt-small">{t(YOURS[list.data.mode])}</p>}
      {editing === 'new' && <SharedForm base={base} onDone={done} onCancel={() => setEditing(null)} />}
      {!list.data && !list.error && <p className="tt-muted">{t('app.loading')}</p>}
      {list.data?.accounts.length === 0 && editing !== 'new' && (
        <p className="tt-muted">{t('settings.actions.none')}</p>
      )}
      {list.data && list.data.accounts.length > 0 && (
        <ul className="tt-rows">
          {list.data.accounts.map((a) =>
            editing === a.id ? (
              <li key={a.id} className="tt-row tt-row-open">
                <SharedForm base={base} account={a} onDone={done} onCancel={() => setEditing(null)} />
              </li>
            ) : (
              <SharedRow
                key={a.id}
                base={base}
                account={a}
                admin={admin}
                onEdit={() => setEditing(a.id)}
                onRemoved={done}
              />
            ),
          )}
        </ul>
      )}
      {admin && <Modes base={base} onChanged={list.reload} />}
    </section>
  )
}

function SharedRow({
  base,
  account: a,
  admin,
  onEdit,
  onRemoved,
}: {
  base: string
  account: SharedCliAccount
  admin: boolean
  onEdit: () => void
  onRemoved: () => void
}) {
  const [test, setTest] = useState<Test | null>(null)
  const action = useAction()
  const t = useT()
  const title = `${a.name} · ${a.label}`
  const run = () => {
    setTest(null)
    action.run(async () => setTest(await api<Test>(`${base}/${a.id}/test`, { method: 'POST', body: {} })))
  }
  const remove = () => action.run(() => api(`${base}/${a.id}`, { method: 'DELETE' })).then((ok) => ok && onRemoved())
  return (
    <li className="tt-row" aria-label={title}>
      <div className="tt-row-main">
        <strong>{title}</strong>
        <span className="tt-muted tt-small">
          <code>{a.base_url}</code>
          {a.model && (
            <>
              {' · '}
              {t.rich('settings.sharedCli.model', { model: <code>{a.model}</code> })}
            </>
          )}
          {' · '}
          {t('settings.sharedCli.keySet')}
        </span>
        {test && (
          <span className={test.ok ? 'tt-ok tt-small' : 'tt-error tt-small'} role={test.ok ? 'status' : 'alert'}>
            {test.ok ? t('settings.sharedCli.keyAccepted') : codeText(test.code, test.params, test.message)}
          </span>
        )}
        <Alert error={action.error} />
        <CliLimits
          account={{
            id: String(a.id),
            cli: a.cli,
            slot: 0,
            label: a.label,
            name: a.name,
            available: true,
            state: 'connected',
            auth_method: 'api_key',
            limits: a.limits,
            limits_at: a.limits_at,
            login: null,
          }}
        />
      </div>
      {admin && (
        <div className="tt-row-actions">
          <button
            type="button"
            className="tt-button"
            aria-label={t('settings.actions.testNamed', { name: title })}
            disabled={action.busy}
            onClick={run}
          >
            {t('settings.actions.test')}
          </button>
          <button
            type="button"
            className="tt-button"
            aria-label={t('settings.actions.editNamed', { name: title })}
            onClick={onEdit}
          >
            {t('settings.actions.edit')}
          </button>
          <ConfirmButton
            label={t('settings.actions.remove')}
            name={title}
            question={t('settings.sharedCli.removeQuestion', { name: title })}
            confirm={t('settings.actions.removeConfirm')}
            disabled={action.busy}
            onConfirm={remove}
          />
        </div>
      )}
    </li>
  )
}

function SharedForm({
  base,
  account,
  onDone,
  onCancel,
}: {
  base: string
  account?: SharedCliAccount
  onDone: () => void
  onCancel: () => void
}) {
  const [form, setForm] = useState({
    cli: (account?.cli ?? 'claude') as CliName,
    label: account?.label ?? '',
    base_url: account?.base_url ?? '',
    model: account?.model ?? '',
    api_key: '',
  })
  const set = (key: keyof typeof form) => (e: { target: { value: string } }) =>
    setForm((f) => ({ ...f, [key]: e.target.value }))
  const action = useAction()
  const t = useT()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    const body: Record<string, string> = { label: form.label.trim(), model: form.model.trim() }
    if (form.base_url.trim()) body.base_url = form.base_url.trim()
    if (form.api_key.trim()) body.api_key = form.api_key.trim()
    const ok = await action.run(() =>
      account
        ? api(`${base}/${account.id}`, { method: 'PATCH', body })
        : api(base, { method: 'POST', body: { cli: form.cli, ...body } }),
    )
    if (ok) onDone()
  }
  return (
    <form
      className="tt-form"
      onSubmit={submit}
      aria-label={
        account
          ? t('settings.actions.editNamed', { name: `${account.name} · ${account.label}` })
          : t('settings.sharedCli.newAccount')
      }
    >
      {!account && (
        <label>
          {t('settings.sharedCli.cli')}
          <select value={form.cli} onChange={set('cli')}>
            {(Object.keys(CLI_PRODUCTS) as CliName[]).map((cli) => (
              <option key={cli} value={cli}>
                {CLI_PRODUCTS[cli]}
              </option>
            ))}
          </select>
        </label>
      )}
      <label>
        {t('settings.actions.name')}
        <input
          value={form.label}
          onChange={set('label')}
          required
          maxLength={60}
          placeholder={t('settings.sharedCli.namePlaceholder')}
        />
      </label>
      <label>
        {t('settings.sharedCli.address')}
        <input
          type="url"
          value={form.base_url}
          onChange={set('base_url')}
          placeholder={DEFAULT_URL[form.cli]}
          spellCheck={false}
        />
      </label>
      <span className="tt-hint">{t('settings.sharedCli.addressHint')}</span>
      <label>
        {t('settings.actions.model')}
        <input
          value={form.model}
          onChange={set('model')}
          maxLength={128}
          spellCheck={false}
          placeholder={t('settings.actions.optional')}
        />
      </label>
      <span className="tt-hint">{t('settings.sharedCli.modelHint')}</span>
      <label>
        {account ? t('settings.actions.newApiKey') : t('settings.actions.apiKey')}
        <input
          type="password"
          value={form.api_key}
          onChange={set('api_key')}
          required={!account}
          autoComplete="new-password"
          spellCheck={false}
          placeholder={account ? t('settings.actions.keepKey') : ''}
        />
      </label>
      <span className="tt-hint">{t('settings.sharedCli.keyHint')}</span>
      <Alert error={action.error} />
      <div className="tt-row-actions">
        <button type="submit" className="tt-button tt-primary" disabled={action.busy}>
          {account ? t('settings.actions.save') : t('settings.actions.add')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}

// Which accounts members' turns use: the workspace's default, and each member's own.
function Modes({ base, onChanged }: { base: string; onChanged: () => void }) {
  const modes = useLoad(() => api<CliMemberModes>(`${base}/modes`), [base])
  const action = useAction()
  const t = useT()
  const save = (body: unknown) =>
    action
      .run(() => api(`${base}/modes`, { method: 'PATCH', body }))
      .then((ok) => {
        if (!ok) return
        modes.reload()
        onChanged()
      })
  if (!modes.data) return <Alert error={modes.error} />
  return (
    <div className="tt-cli-modes">
      <h3>{t('settings.sharedCli.modesTitle')}</h3>
      <label>
        {t('settings.sharedCli.defaultMode')}
        <select
          value={modes.data.default_mode}
          disabled={action.busy}
          onChange={(e) => save({ default_mode: e.target.value })}
        >
          {(Object.keys(MODE_LABEL) as CliAccountMode[]).map((m) => (
            <option key={m} value={m}>
              {t(MODE_LABEL[m])}
            </option>
          ))}
        </select>
      </label>
      <ul className="tt-rows">
        {modes.data.members.map((m) => (
          <li key={m.user_id} className="tt-row" aria-label={m.name}>
            <div className="tt-row-main">
              <strong>{m.name}</strong>
              <span className="tt-muted tt-small">{m.email}</span>
            </div>
            <select
              aria-label={t('settings.sharedCli.accountsFor', { name: m.name })}
              value={m.mode ?? ''}
              disabled={action.busy}
              onChange={(e) => save({ members: { [m.user_id]: e.target.value || null } })}
            >
              <option value="">{t('settings.sharedCli.theDefault')}</option>
              {(Object.keys(MODE_LABEL) as CliAccountMode[]).map((mode) => (
                <option key={mode} value={mode}>
                  {t(MODE_LABEL[mode])}
                </option>
              ))}
            </select>
          </li>
        ))}
      </ul>
      <Alert error={action.error} />
    </div>
  )
}
