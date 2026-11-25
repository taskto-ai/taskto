// Integrations: the workspace's connections to accounts at outside providers (payments: Stripe,
// Square), for its admins. A credential goes in and never comes back: the API answers "set" or
// "not set", so the form only ever offers to replace it. The admin also says which spaces may use
// an integration and the largest amount and the currencies one payment may have; no app can change
// either (docs/integrations.md).

import { type FormEvent, useState } from 'react'
import {
  api,
  codeText,
  type Integration,
  type IntegrationCheck,
  type IntegrationProvider,
  type IntegrationRegistry,
  type Space,
  ws,
} from '../api'
import { type Format, has, type Key, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Alert, ConfirmButton, CopyButton, useAction } from './parts'

type Translate = ReturnType<typeof useT>

// The labels the server's closed lists have in the catalog, or the code itself for one it doesn't.
function label(t: Translate, group: string, code: string): string {
  const key = `settings.integrations.${group}.${code}`
  return has(key) ? t(key as Key) : code
}

// The number of decimals a currency has, from the formatter's own table.
function decimals(f: Format, currency: string): number {
  return new Intl.NumberFormat(f.locale, { style: 'currency', currency }).resolvedOptions().maximumFractionDigits ?? 2
}

// An amount in a currency's smallest unit, written as people write money.
function money(f: Format, minor: number, currency: string): string {
  return f.number(minor / 10 ** decimals(f, currency), { style: 'currency', currency })
}

// What a ceiling comes to in each of the currencies: the same number of smallest units is not the
// same sum in euros and in yen, so the page says each.
function amounts(f: Format, minor: number, currencies: string[]): string {
  return f.list(currencies.map((c) => money(f, minor, c)))
}

export function IntegrationsTab({ workspace }: { workspace: number }) {
  const t = useT()
  const base = `${ws(workspace)}/integrations`
  const registry = useLoad(() => api<IntegrationRegistry>(`${ws(workspace)}/integration-providers`), [workspace])
  const list = useLoad(() => api<{ integrations: Integration[] }>(base).then((r) => r.integrations), [base])
  const spaces = useLoad(
    () =>
      api<{ spaces: Space[] }>(`${ws(workspace)}/spaces?all=true`).then((r) => r.spaces.filter((s) => !s.archived_at)),
    [workspace],
  )
  const [editing, setEditing] = useState<number | 'new' | null>(null)
  const done = () => {
    setEditing(null)
    list.reload()
  }

  return (
    <div className="tt-settings">
      <section className="tt-panel" aria-label={t('settings.integrations.title')}>
        <div className="tt-pane-head">
          <h2>{t('settings.integrations.title')}</h2>
          {registry.data && editing !== 'new' && (
            <button type="button" className="tt-button" onClick={() => setEditing('new')}>
              {t('settings.integrations.add')}
            </button>
          )}
        </div>
        <p className="tt-muted tt-small">{t('settings.integrations.intro')}</p>
        <p className="tt-muted tt-small">{t('settings.integrations.backupHint')}</p>
        {registry.data && !registry.data.webhooks_available && (
          <p className="tt-muted tt-small" role="status">
            {t('settings.integrations.webhooksUnavailable')}
          </p>
        )}
        <Alert error={registry.error ?? list.error ?? spaces.error} />
        {registry.data && editing === 'new' && (
          <IntegrationForm base={base} registry={registry.data} onDone={done} onCancel={() => setEditing(null)} />
        )}
        {!list.data && !list.error && <p className="tt-muted">{t('app.loading')}</p>}
        {list.data?.length === 0 && editing !== 'new' && <p className="tt-muted">{t('settings.integrations.none')}</p>}
        {list.data && list.data.length > 0 && registry.data && (
          <ul className="tt-rows">
            {list.data.map((i) =>
              editing === i.id ? (
                <li key={i.id} className="tt-row tt-row-open">
                  <IntegrationForm
                    base={base}
                    registry={registry.data as IntegrationRegistry}
                    integration={i}
                    onDone={done}
                    onCancel={() => setEditing(null)}
                  />
                </li>
              ) : (
                <IntegrationRow
                  key={i.id}
                  base={base}
                  integration={i}
                  spaces={spaces.data ?? []}
                  onEdit={() => setEditing(i.id)}
                  onChanged={list.reload}
                />
              ),
            )}
          </ul>
        )}
      </section>
    </div>
  )
}

function IntegrationRow({
  base,
  integration: i,
  spaces,
  onEdit,
  onChanged,
}: {
  base: string
  integration: Integration
  spaces: Space[]
  onEdit: () => void
  onChanged: () => void
}) {
  const t = useT()
  const f = useFormat()
  const action = useAction()
  const [check, setCheck] = useState<IntegrationCheck | null>(null)
  const [choosing, setChoosing] = useState(false)
  const url = `${base}/${i.id}`
  const names = i.spaces.map((id) => spaces.find((s) => s.id === id)).filter((s): s is Space => s !== undefined)

  const runCheck = () => {
    setCheck(null)
    action.run(async () => setCheck(await api<IntegrationCheck>(`${url}/check`, { method: 'POST' })))
  }
  const toggle = () =>
    action.run(() => api(url, { method: 'PATCH', body: { disabled: !i.disabled } })).then((ok) => ok && onChanged())
  const remove = () => action.run(() => api(url, { method: 'DELETE' })).then((ok) => ok && onChanged())

  return (
    <li className="tt-row" aria-label={i.display_name}>
      <div className="tt-row-main">
        <strong>{i.display_name}</strong>
        <span className="tt-muted tt-small">
          {label(t, 'provider', i.provider)}
          {' · '}
          {label(t, 'kind', i.kind)}
          {' · '}
          {label(t, 'mode', i.mode)}
          {i.disabled && ` · ${t('settings.integrations.switchedOff')}`}
        </span>
        <span className="tt-muted tt-small">
          {i.secrets.api_key === 'set'
            ? t('settings.integrations.secretSet', { name: label(t, 'secret', 'api_key') })
            : t('settings.integrations.secretNotSet', { name: label(t, 'secret', 'api_key') })}
          {' · '}
          {i.secrets.webhook_secret === 'set'
            ? t('settings.integrations.secretSet', { name: label(t, 'secret', 'webhook_secret') })
            : t('settings.integrations.secretNotSet', { name: label(t, 'secret', 'webhook_secret') })}
        </span>
        <span className="tt-muted tt-small">
          {t('settings.integrations.limits', {
            amounts: amounts(f, i.max_amount_minor, i.currencies),
          })}
        </span>
        <span className="tt-muted tt-small">
          {names.length > 0
            ? t('settings.integrations.allowedIn', { spaces: f.list(names.map((s) => s.name)) })
            : t('settings.integrations.allowedNowhere')}
        </span>
        <span className="tt-muted tt-small">
          {i.webhook_url ? (
            <>
              {t('settings.integrations.webhookAddress')} <code>{i.webhook_url}</code>
            </>
          ) : (
            t('settings.integrations.polling')
          )}
        </span>
        {action.busy && !check && <span className="tt-muted tt-small">{t('settings.integrations.checking')}</span>}
        {check?.ok && (
          <span className="tt-ok tt-small" role="status">
            {t('settings.integrations.checkOk')}
          </span>
        )}
        {check && !check.ok && (
          <span className="tt-error tt-small" role="alert">
            {codeText(check.code, check.params, '')}
          </span>
        )}
        <Alert error={action.error} />
        {choosing && (
          <SpacesForm
            url={url}
            integration={i}
            spaces={spaces}
            onDone={() => {
              setChoosing(false)
              onChanged()
            }}
            onCancel={() => setChoosing(false)}
          />
        )}
      </div>
      <div className="tt-row-actions">
        {i.webhook_url && <CopyButton text={i.webhook_url} name={i.display_name} />}
        <button
          type="button"
          className="tt-button"
          aria-label={t('settings.actions.testNamed', { name: i.display_name })}
          disabled={action.busy}
          onClick={runCheck}
        >
          {t('settings.actions.test')}
        </button>
        <button
          type="button"
          className="tt-button"
          aria-label={t('settings.integrations.spacesNamed', { name: i.display_name })}
          onClick={() => setChoosing(true)}
        >
          {t('settings.integrations.spaces')}
        </button>
        <button
          type="button"
          className="tt-button"
          aria-label={t('settings.actions.editNamed', { name: i.display_name })}
          onClick={onEdit}
        >
          {t('settings.actions.edit')}
        </button>
        <button
          type="button"
          className="tt-button"
          aria-label={t(i.disabled ? 'settings.integrations.switchOnNamed' : 'settings.integrations.switchOffNamed', {
            name: i.display_name,
          })}
          disabled={action.busy}
          onClick={toggle}
        >
          {i.disabled ? t('settings.integrations.switchOn') : t('settings.integrations.switchOff')}
        </button>
        <ConfirmButton
          label={t('settings.actions.remove')}
          name={i.display_name}
          question={t('settings.integrations.removeQuestion', { name: i.display_name })}
          confirm={t('settings.actions.removeConfirm')}
          disabled={action.busy}
          onConfirm={remove}
        />
      </div>
    </li>
  )
}

// Which spaces may use the integration: the admin's own private space and the shared ones, since
// those are the ones an admin may allow without being let into anyone else's.
function SpacesForm({
  url,
  integration,
  spaces,
  onDone,
  onCancel,
}: {
  url: string
  integration: Integration
  spaces: Space[]
  onDone: () => void
  onCancel: () => void
}) {
  const t = useT()
  const action = useAction()
  const [chosen, setChosen] = useState<number[]>(integration.spaces)
  const flip = (id: number) => setChosen((c) => (c.includes(id) ? c.filter((x) => x !== id) : [...c, id]))
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    const ok = await action.run(() => api(`${url}/spaces`, { method: 'PUT', body: { spaces: chosen } }))
    if (ok) onDone()
  }
  return (
    <form
      className="tt-form"
      onSubmit={submit}
      aria-label={t('settings.integrations.spacesNamed', { name: integration.display_name })}
    >
      <p className="tt-hint">{t('settings.integrations.spacesHint')}</p>
      <fieldset className="tt-checks">
        <legend className="tt-small">{t('settings.integrations.spaces')}</legend>
        {spaces.map((s) => (
          <label key={s.id}>
            <input type="checkbox" checked={chosen.includes(s.id)} onChange={() => flip(s.id)} />
            {s.kind === 'private' ? t('settings.integrations.privateSpace', { name: s.name }) : s.name}
          </label>
        ))}
      </fieldset>
      <Alert error={action.error} />
      <div className="tt-row-actions">
        <button type="submit" className="tt-button tt-primary" disabled={action.busy}>
          {t('settings.actions.save')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}

function IntegrationForm({
  base,
  registry,
  integration,
  onDone,
  onCancel,
}: {
  base: string
  registry: IntegrationRegistry
  integration?: Integration
  onDone: () => void
  onCancel: () => void
}) {
  const t = useT()
  const f = useFormat()
  const action = useAction()
  const [form, setForm] = useState({
    provider: integration?.provider ?? registry.providers[0]?.id ?? '',
    name: integration?.name ?? '',
    display_name: integration?.display_name ?? '',
    max_amount_minor: String(integration?.max_amount_minor ?? ''),
  })
  const [settings, setSettings] = useState<Record<string, string>>(integration?.settings ?? {})
  const [secrets, setSecrets] = useState<Record<string, string>>({})
  const [currencies, setCurrencies] = useState<string[]>(integration?.currencies ?? [])
  const provider: IntegrationProvider | undefined = registry.providers.find((p) => p.id === form.provider)
  const set = (key: keyof typeof form) => (e: { target: { value: string } }) =>
    setForm((x) => ({ ...x, [key]: e.target.value }))
  const ceiling = Number(form.max_amount_minor)
  const preview =
    Number.isSafeInteger(ceiling) && ceiling > 0 && currencies.length > 0 ? amounts(f, ceiling, currencies) : null

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (!provider) return
    // Only the secrets that were typed: an empty box keeps the stored one.
    const typed = Object.fromEntries(
      Object.entries(secrets)
        .map(([k, v]) => [k, v.trim()] as const)
        .filter(([, v]) => v !== ''),
    )
    const wanted = Object.fromEntries(provider.settings.map((s) => [s.name, (settings[s.name] ?? '').trim()]))
    const body: Record<string, unknown> = {
      display_name: form.display_name.trim(),
      settings: Object.fromEntries(Object.entries(wanted).filter(([, v]) => v !== '')),
      max_amount_minor: ceiling,
      currencies,
    }
    if (Object.keys(typed).length > 0) body.secrets = typed
    const ok = await action.run(() =>
      integration
        ? api(`${base}/${integration.id}`, { method: 'PATCH', body })
        : api(base, {
            method: 'POST',
            body: { provider: form.provider, name: form.name.trim(), secrets: typed, ...body },
          }),
    )
    if (ok) onDone()
  }

  return (
    <form
      className="tt-form"
      onSubmit={submit}
      aria-label={
        integration
          ? t('settings.actions.editNamed', { name: integration.display_name })
          : t('settings.integrations.newIntegration')
      }
    >
      {!integration && (
        <>
          <label>
            {t('settings.integrations.providerLabel')}
            <select value={form.provider} onChange={set('provider')} required>
              {registry.providers.map((p) => (
                <option key={p.id} value={p.id}>
                  {label(t, 'provider', p.id)}
                </option>
              ))}
            </select>
          </label>
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
              aria-describedby="tt-integration-name-hint"
            />
          </label>
          <span id="tt-integration-name-hint" className="tt-hint">
            {t('settings.providers.nameHint')}
          </span>
        </>
      )}
      <label>
        {t('settings.providers.displayName')}
        <input value={form.display_name} onChange={set('display_name')} maxLength={100} placeholder={form.name} />
      </label>
      {provider?.settings.map((spec) => (
        <div key={spec.name} className="tt-field">
          <label htmlFor={`tt-integration-setting-${spec.name}`}>{label(t, 'setting', spec.name)}</label>
          {spec.options.length > 0 ? (
            <select
              id={`tt-integration-setting-${spec.name}`}
              value={settings[spec.name] ?? ''}
              required={spec.required}
              onChange={(e) => setSettings((x) => ({ ...x, [spec.name]: e.target.value }))}
            >
              <option value="" />
              {spec.options.map((o) => (
                <option key={o} value={o}>
                  {label(t, 'settingOption', o)}
                </option>
              ))}
            </select>
          ) : (
            <input
              id={`tt-integration-setting-${spec.name}`}
              value={settings[spec.name] ?? ''}
              required={spec.required}
              maxLength={64}
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => setSettings((x) => ({ ...x, [spec.name]: e.target.value }))}
            />
          )}
        </div>
      ))}
      {provider?.secrets.map((spec) => {
        const stored = integration?.secrets[spec.name] === 'set'
        const name = label(t, 'secret', spec.name)
        const hint = `tt-integration-hint-${spec.name}`
        return (
          <div key={spec.name} className="tt-field">
            <label>
              {stored ? t('settings.integrations.newSecret', { name }) : name}
              <input
                type="password"
                value={secrets[spec.name] ?? ''}
                onChange={(e) => setSecrets((x) => ({ ...x, [spec.name]: e.target.value }))}
                required={spec.required && !stored}
                autoComplete="new-password"
                spellCheck={false}
                placeholder={
                  stored
                    ? t('settings.integrations.keepSecret')
                    : spec.required
                      ? undefined
                      : t('settings.actions.optional')
                }
                aria-describedby={hint}
              />
            </label>
            <span id={hint} className="tt-hint">
              {stored ? t('settings.integrations.secretHintStored') : t('settings.integrations.secretHint')}
            </span>
          </div>
        )
      })}
      <label>
        {t('settings.integrations.ceiling')}
        <input
          type="number"
          inputMode="numeric"
          min={1}
          max={registry.max_amount_minor}
          step={1}
          value={form.max_amount_minor}
          onChange={set('max_amount_minor')}
          required
          aria-describedby="tt-integration-ceiling-hint"
        />
      </label>
      <span id="tt-integration-ceiling-hint" className="tt-hint">
        {t('settings.integrations.ceilingHint')}
        {preview && ` ${t('settings.integrations.ceilingPreview', { amounts: preview })}`}
      </span>
      <fieldset className="tt-checks">
        <legend className="tt-small">{t('settings.integrations.currencies')}</legend>
        {registry.currencies.map((c) => (
          <label key={c}>
            <input
              type="checkbox"
              checked={currencies.includes(c)}
              onChange={() => setCurrencies((cs) => (cs.includes(c) ? cs.filter((x) => x !== c) : [...cs, c]))}
            />
            {c}
          </label>
        ))}
      </fieldset>
      <Alert error={action.error} />
      <div className="tt-row-actions">
        <button type="submit" className="tt-button tt-primary" disabled={action.busy || currencies.length === 0}>
          {integration ? t('settings.actions.save') : t('settings.actions.add')}
        </button>
        <button type="button" className="tt-button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
      </div>
    </form>
  )
}
