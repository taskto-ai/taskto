// Model provider accounts: the workspace's (its admins manage them, its members use them)
// and the viewer's own (only they see and use them, in any workspace). An API key goes in
// and never comes back: the API answers "set" or "not set", so the form only ever offers to
// replace or remove it.

import { type FormEvent, useState } from 'react'
import { api, codeText, type Model, type ProviderAccount, type ProviderCheck, ws } from '../api'
import { useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Alert, ConfirmButton, useAction } from './parts'

export function ProvidersTab({ workspace, admin }: { workspace: number; admin: boolean }) {
  const t = useT()
  return (
    <div className="tt-settings">
      <Accounts
        title={t('settings.providers.workspaceTitle')}
        base={`${ws(workspace)}/providers`}
        editable={admin}
        intro={admin ? t('settings.providers.workspaceIntroAdmin') : t('settings.providers.workspaceIntro')}
      />
      <Accounts
        title={t('settings.providers.ownTitle')}
        base="/api/me/providers"
        editable
        intro={t('settings.providers.ownIntro')}
      />
    </div>
  )
}

function Accounts({ title, base, editable, intro }: { title: string; base: string; editable: boolean; intro: string }) {
  const accounts = useLoad(() => api<{ providers: ProviderAccount[] }>(base).then((r) => r.providers), [base])
  const [editing, setEditing] = useState<number | 'new' | null>(null)
  const t = useT()
  const done = () => {
    setEditing(null)
    accounts.reload()
  }
  return (
    <section className="tt-panel" aria-label={title}>
      <div className="tt-pane-head">
        <h2>{title}</h2>
        {editable && editing !== 'new' && (
          <button type="button" className="tt-button" onClick={() => setEditing('new')}>
            {t('settings.providers.add')}
          </button>
        )}
      </div>
      <p className="tt-muted tt-small">{intro}</p>
      <Alert error={accounts.error} />
      {editing === 'new' && <AccountForm base={base} onDone={done} onCancel={() => setEditing(null)} />}
      {!accounts.data && !accounts.error && <p className="tt-muted">{t('app.loading')}</p>}
      {accounts.data?.length === 0 && editing !== 'new' && <p className="tt-muted">{t('settings.actions.none')}</p>}
      {accounts.data && accounts.data.length > 0 && (
        <ul className="tt-rows">
          {accounts.data.map((a) =>
            editing === a.id ? (
              <li key={a.id} className="tt-row tt-row-open">
                <AccountForm base={base} account={a} onDone={done} onCancel={() => setEditing(null)} />
              </li>
            ) : (
              <AccountRow
                key={a.id}
                base={base}
                account={a}
                editable={editable}
                onEdit={() => setEditing(a.id)}
                onRemoved={done}
              />
            ),
          )}
        </ul>
      )}
    </section>
  )
}

function AccountRow({
  base,
  account: a,
  editable,
  onEdit,
  onRemoved,
}: {
  base: string
  account: ProviderAccount
  editable: boolean
  onEdit: () => void
  onRemoved: () => void
}) {
  const [check, setCheck] = useState<ProviderCheck | null>(null)
  const action = useAction()
  const t = useT()
  const f = useFormat()
  const test = () => {
    setCheck(null)
    action.run(async () => setCheck(await api<ProviderCheck>(`${base}/${a.id}/test`, { method: 'POST' })))
  }
  const remove = () => action.run(() => api(`${base}/${a.id}`, { method: 'DELETE' })).then((ok) => ok && onRemoved())

  return (
    <li className="tt-row" aria-label={a.display_name}>
      <div className="tt-row-main">
        <strong>{a.display_name}</strong>
        <span className="tt-muted tt-small">
          <code>{a.provider}</code>
          {' · '}
          <code>{a.base_url}</code>
        </span>
        <span className="tt-muted tt-small">
          {a.api_key === 'set' ? t('settings.providers.keySet') : t('settings.providers.keyNotSet')}
          {' · '}
          {f.list(a.models.map((m) => m.name || m.id))}
        </span>
        {action.busy && !check && <span className="tt-muted tt-small">{t('settings.providers.testing')}</span>}
        {check?.ok && (
          <span className="tt-ok tt-small" role="status">
            {check.missing?.length
              ? t('settings.providers.answeredMissing', { models: f.list(check.missing) })
              : t('settings.providers.answeredAll')}
          </span>
        )}
        {check && !check.ok && (
          <span className="tt-error tt-small" role="alert">
            {codeText(check.code, check.params, check.error ?? '')}
          </span>
        )}
        <Alert error={action.error} />
      </div>
      {editable && (
        <div className="tt-row-actions">
          <button
            type="button"
            className="tt-button"
            aria-label={t('settings.actions.testNamed', { name: a.display_name })}
            disabled={action.busy}
            onClick={test}
          >
            {t('settings.actions.test')}
          </button>
          <button
            type="button"
            className="tt-button"
            aria-label={t('settings.actions.editNamed', { name: a.display_name })}
            onClick={onEdit}
          >
            {t('settings.actions.edit')}
          </button>
          <ConfirmButton
            label={t('settings.actions.remove')}
            name={a.display_name}
            question={t('settings.providers.removeQuestion', { name: a.display_name })}
            confirm={t('settings.actions.removeConfirm')}
            disabled={action.busy}
            onConfirm={remove}
          />
        </div>
      )}
    </li>
  )
}

// One model ID per line; a model kept from before keeps its other settings.
function parseModels(text: string, before: Model[]): Model[] {
  const ids = [...new Set(text.split('\n').map((l) => l.trim()))].filter(Boolean)
  return ids.map((id) => before.find((m) => m.id === id) ?? { id })
}

function AccountForm({
  base,
  account,
  onDone,
  onCancel,
}: {
  base: string
  account?: ProviderAccount
  onDone: () => void
  onCancel: () => void
}) {
  const [form, setForm] = useState({
    name: account?.name ?? '',
    display_name: account?.display_name ?? '',
    base_url: account?.base_url ?? '',
    models: account?.models.map((m) => m.id).join('\n') ?? '',
    api_key: '',
    remove_key: false,
  })
  const set = (key: keyof typeof form) => (e: { target: { value: string } }) =>
    setForm((f) => ({ ...f, [key]: e.target.value }))
  const action = useAction()
  const t = useT()
  const hasKey = account?.api_key === 'set'

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    const models = parseModels(form.models, account?.models ?? [])
    const key = form.api_key.trim()
    const body: Record<string, unknown> = {
      display_name: form.display_name.trim(),
      base_url: form.base_url.trim(),
      models,
    }
    if (key) body.api_key = key
    else if (form.remove_key) body.api_key = null
    const ok = await action.run(() =>
      account
        ? api(`${base}/${account.id}`, { method: 'PATCH', body })
        : api(base, { method: 'POST', body: { name: form.name.trim(), ...body } }),
    )
    if (ok) onDone()
  }

  return (
    <form
      className="tt-form"
      onSubmit={submit}
      aria-label={
        account ? t('settings.actions.editNamed', { name: account.display_name }) : t('settings.providers.newAccount')
      }
    >
      {!account && (
        <>
          <label>
            {t('settings.actions.name')}
            <input
              value={form.name}
              onChange={set('name')}
              required
              maxLength={40}
              pattern="[a-z0-9][a-z0-9._\-]*"
              autoComplete="off"
              spellCheck={false}
              aria-describedby="tt-name-hint"
            />
          </label>
          <span id="tt-name-hint" className="tt-hint">
            {t('settings.providers.nameHint')}
          </span>
        </>
      )}
      <label>
        {t('settings.providers.displayName')}
        <input value={form.display_name} onChange={set('display_name')} maxLength={100} placeholder={form.name} />
      </label>
      <label>
        {t('settings.actions.baseUrl')}
        <input
          type="url"
          value={form.base_url}
          onChange={set('base_url')}
          required
          placeholder="http://localhost:11434/v1"
          spellCheck={false}
          aria-describedby="tt-url-hint"
        />
      </label>
      <span id="tt-url-hint" className="tt-hint">
        {t('settings.providers.urlHint')}
      </span>
      <label>
        {hasKey ? t('settings.actions.newApiKey') : t('settings.actions.apiKey')}
        <input
          type="password"
          value={form.api_key}
          onChange={set('api_key')}
          autoComplete="new-password"
          spellCheck={false}
          placeholder={hasKey ? t('settings.actions.keepKey') : t('settings.actions.optional')}
          aria-describedby="tt-key-hint"
        />
      </label>
      <span id="tt-key-hint" className="tt-hint">
        {hasKey ? t('settings.providers.keyHintStored') : t('settings.providers.keyHint')}
      </span>
      {hasKey && (
        <label className="tt-check">
          <input
            type="checkbox"
            checked={form.remove_key}
            disabled={form.api_key.trim() !== ''}
            onChange={(e) => setForm((f) => ({ ...f, remove_key: e.target.checked }))}
          />
          {t('settings.providers.removeKey')}
        </label>
      )}
      <label>
        {t('settings.providers.models')}
        <textarea
          value={form.models}
          onChange={set('models')}
          required
          rows={3}
          spellCheck={false}
          placeholder="llama3.1:8b"
          aria-describedby="tt-models-hint"
        />
      </label>
      <span id="tt-models-hint" className="tt-hint">
        {t('settings.providers.modelsHint')}
      </span>
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
