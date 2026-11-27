import { type FormEvent, useId, useState } from 'react'
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
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { Checkbox } from '../ui/Checkbox'
import { type ConfirmOptions, useConfirm } from '../ui/ConfirmDialog'
import { Dialog } from '../ui/Dialog'
import { EmptyState } from '../ui/EmptyState'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { MenuItem, MenuSeparator, MoreMenu } from '../ui/Menu'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { Select } from '../ui/Select'
import { Alert, CopyButton, FormError, Loading, Section, SettingsTab, useAction, useRefocus } from './parts'

type Translate = ReturnType<typeof useT>

function label(t: Translate, group: string, code: string): string {
  const key = `settings.integrations.${group}.${code}`
  return has(key) ? t(key as Key) : code
}

type Exponents = Record<string, number>

function money(f: Format, exponents: Exponents, minor: number, currency: string): string {
  const digits = exponents[currency] ?? 2
  return f.number(minor / 10 ** digits, {
    style: 'currency',
    currency,
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  })
}

function amounts(f: Format, exponents: Exponents, minor: number, currencies: string[]): string {
  return f.list(currencies.map((c) => money(f, exponents, minor, c)))
}

export function IntegrationsTab({ workspace }: { workspace: number }) {
  const t = useT()
  const base = `${ws(workspace)}/integrations`
  const registry = useLoad(() => api<IntegrationRegistry>(`${ws(workspace)}/integration-providers`), [workspace])
  const list = useLoad(() => api<{ integrations: Integration[] }>(base).then((r) => r.integrations), [base])
  const spaces = useLoad(
    () => api<{ spaces: Space[] }>(`${ws(workspace)}/spaces?all=true`).then((r) => r.spaces),
    [workspace],
  )
  const [editing, setEditing] = useState<number | 'new' | null>(null)
  const [confirmDialog, confirm] = useConfirm()
  const refocus = useRefocus(list.data)
  const done = () => {
    if (list.data?.length === 0) refocus.after()
    setEditing(null)
    list.reload()
  }
  const removed = () => {
    refocus.after()
    list.reload()
  }
  const add = registry.data && (
    <Button icon="plus" onClick={() => setEditing('new')}>
      {t('settings.integrations.add')}
    </Button>
  )
  const empty = list.data?.length === 0
  const integration = typeof editing === 'number' ? list.data?.find((i) => i.id === editing) : undefined
  const title = t('settings.integrations.accounts')

  return (
    <SettingsTab title={t('settings.integrations.title')} help={t('settings.help.integrations')}>
      <Section
        title={title}
        help={t('settings.integrations.intro')}
        actions={!empty && add}
        headingRef={refocus.heading}
      >
        <Alert error={registry.error ?? list.error ?? spaces.error} />
        {!list.data && !list.error && <Loading lines={2} />}
        {empty && (
          <Panel>
            <EmptyState title={t('settings.integrations.none')} actions={add} />
          </Panel>
        )}
        {list.data && list.data.length > 0 && registry.data && (
          <Panel>
            <Rows label={title}>
              {list.data.map((i) => (
                <IntegrationRow
                  key={i.id}
                  base={base}
                  integration={i}
                  exponents={(registry.data as IntegrationRegistry).exponents}
                  spaces={spaces.data ?? []}
                  onEdit={() => setEditing(i.id)}
                  onChanged={list.reload}
                  onRemoved={removed}
                  confirm={confirm}
                />
              ))}
            </Rows>
          </Panel>
        )}
        <Notice tone="neutral" live={false}>
          {t('settings.integrations.backupHint')}
        </Notice>
        {registry.data && !registry.data.webhooks_available && (
          <Notice tone="neutral" live={false}>
            {t('settings.integrations.webhooksUnavailable')}
          </Notice>
        )}
      </Section>
      {registry.data && (editing === 'new' || integration) && (
        <IntegrationForm
          key={integration?.id ?? 'new'}
          base={base}
          registry={registry.data}
          integration={integration}
          onDone={done}
          onCancel={() => setEditing(null)}
        />
      )}
      {confirmDialog}
    </SettingsTab>
  )
}

type Confirm = (options: ConfirmOptions) => Promise<boolean>

function IntegrationRow({
  base,
  integration: i,
  exponents,
  spaces,
  onEdit,
  onChanged,
  onRemoved,
  confirm,
}: {
  base: string
  integration: Integration
  exponents: Exponents
  spaces: Space[]
  onEdit: () => void
  onChanged: () => void
  onRemoved: () => void
  confirm: Confirm
}) {
  const t = useT()
  const f = useFormat()
  const action = useAction()
  const [check, setCheck] = useState<IntegrationCheck | null>(null)
  const [checking, setChecking] = useState(false)
  const [choosing, setChoosing] = useState(false)
  const url = `${base}/${i.id}`
  const names = i.spaces
    .map((id) => spaces.find((s) => s.id === id))
    .filter((s): s is Space => s !== undefined)
    .map((s) =>
      i.approval_spaces.includes(s.id)
        ? t('settings.integrations.approvalSpace', { name: spaceName(t, s) })
        : spaceName(t, s),
    )

  const runCheck = () => {
    setCheck(null)
    setChecking(true)
    action
      .run(async () => setCheck(await api<IntegrationCheck>(`${url}/check`, { method: 'POST' })))
      .finally(() => setChecking(false))
  }
  const toggle = () =>
    action.run(() => api(url, { method: 'PATCH', body: { disabled: !i.disabled } })).then((ok) => ok && onChanged())
  const remove = async () => {
    const yes = await confirm({
      title: t('settings.integrations.removeQuestion', { name: i.display_name }),
      confirmLabel: t('settings.actions.removeConfirm'),
      tone: 'danger',
    })
    if (yes) action.run(() => api(url, { method: 'DELETE' })).then((ok) => ok && onRemoved())
  }

  return (
    <Row
      title={i.display_name}
      actionsBelow
      status={
        <>
          {check?.ok && (
            <Badge tone="success" dot>
              {t('settings.providers.answered')}
            </Badge>
          )}
          {check && !check.ok && (
            <Badge tone="danger" dot>
              {t('settings.providers.failing')}
            </Badge>
          )}
          {i.disabled && <Badge>{t('settings.integrations.switchedOff')}</Badge>}
        </>
      }
      detail={
        <span className="settings-row-extra">
          <span>
            {label(t, 'provider', i.provider)}
            {' · '}
            {label(t, 'kind', i.kind)}
            {' · '}
            {label(t, 'mode', i.mode)}
          </span>
          <span>
            {i.secrets.api_key === 'set'
              ? t('settings.integrations.secretSet', { name: label(t, 'secret', 'api_key') })
              : t('settings.integrations.secretNotSet', { name: label(t, 'secret', 'api_key') })}
            {' · '}
            {i.secrets.webhook_secret === 'set'
              ? t('settings.integrations.secretSet', { name: label(t, 'secret', 'webhook_secret') })
              : t('settings.integrations.secretNotSet', { name: label(t, 'secret', 'webhook_secret') })}
          </span>
          <span>
            {t('settings.integrations.limits', {
              amounts: amounts(f, exponents, i.max_amount_minor, i.currencies),
            })}
          </span>
          <span>
            {names.length > 0 && t('settings.integrations.allowedIn', { spaces: f.list(names) })}
            {names.length === 0 && i.other_spaces === 0 && t('settings.integrations.allowedNowhere')}
            {i.other_spaces > 0 &&
              `${names.length > 0 ? ' ' : ''}${t('settings.integrations.otherSpaces', { count: i.other_spaces })}`}
          </span>
          <span>
            {i.webhook_url ? (
              <>
                {t('settings.integrations.webhookAddress')} <code>{i.webhook_url}</code>
              </>
            ) : (
              t('settings.integrations.polling')
            )}
          </span>
          <span role="status" className="settings-check-status">
            {checking ? t('settings.integrations.checking') : check?.ok ? t('settings.integrations.checkOk') : ''}
          </span>
          {check && !check.ok && (
            <span role="alert" className="settings-error-text">
              {codeText(check.code, check.params, '')}
            </span>
          )}
          {action.error && (
            <span role="alert" className="settings-error-text">
              {action.error}
            </span>
          )}
        </span>
      }
      actions={
        <>
          {i.webhook_url && <CopyButton size="sm" text={i.webhook_url} name={i.display_name} />}
          <Button
            size="sm"
            aria-label={t('settings.actions.testNamed', { name: i.display_name })}
            loading={checking}
            disabled={action.busy}
            onClick={runCheck}
          >
            {t('settings.actions.test')}
          </Button>
          <MoreMenu label={t('settings.moreFor', { name: i.display_name })}>
            <MenuItem icon="edit" onSelect={onEdit}>
              {t('settings.actions.edit')}
            </MenuItem>
            <MenuItem onSelect={() => setChoosing(true)}>{t('settings.integrations.spaces')}</MenuItem>
            <MenuItem disabled={action.busy} onSelect={toggle}>
              {i.disabled ? t('settings.integrations.switchOn') : t('settings.integrations.switchOff')}
            </MenuItem>
            <MenuSeparator />
            <MenuItem tone="danger" disabled={action.busy} onSelect={remove}>
              {t('settings.actions.remove')}
            </MenuItem>
          </MoreMenu>
        </>
      }
    >
      {choosing && (
        <SpacesForm
          url={url}
          integration={i}
          spaces={spaces.filter((s) => !s.archived_at || i.spaces.includes(s.id))}
          onDone={() => {
            setChoosing(false)
            onChanged()
          }}
          onCancel={() => setChoosing(false)}
        />
      )}
    </Row>
  )
}

function spaceName(t: Translate, s: Space): string {
  if (s.kind === 'private') return t('settings.integrations.privateSpace', { name: s.name })
  if (s.archived_at) return t('settings.integrations.archivedSpace', { name: s.name })
  return s.name
}

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
  const formId = useId()
  const [chosen, setChosen] = useState<number[]>(integration.spaces)
  const [kept, setKept] = useState<number[]>([])
  const flip = (id: number) => setChosen((c) => (c.includes(id) ? c.filter((x) => x !== id) : [...c, id]))
  const flipKept = (id: number) => setKept((k) => (k.includes(id) ? k.filter((x) => x !== id) : [...k, id]))
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    const keep = kept.filter((id) => chosen.includes(id))
    const body = keep.length > 0 ? { spaces: chosen, keep } : { spaces: chosen }
    const ok = await action.run(() => api(`${url}/spaces`, { method: 'PUT', body }))
    if (ok) onDone()
  }
  const name = t('settings.integrations.spacesNamed', { name: integration.display_name })
  return (
    <Dialog
      open
      onClose={onCancel}
      dismissible={!action.busy}
      title={name}
      actions={
        <>
          <Button onClick={onCancel} disabled={action.busy}>
            {t('common.cancel')}
          </Button>
          <Button type="submit" form={formId} variant="primary" loading={action.busy}>
            {t('settings.actions.save')}
          </Button>
        </>
      }
    >
      <form id={formId} className="settings-form" onSubmit={submit} aria-label={name}>
        <p className="settings-text">{t('settings.integrations.spacesHint')}</p>
        <fieldset className="settings-fieldset">
          <legend>{t('settings.integrations.spaces')}</legend>
          {spaces.map((s) => {
            const byApproval = integration.approval_spaces.includes(s.id)
            return (
              <div key={s.id}>
                <Checkbox label={spaceName(t, s)} checked={chosen.includes(s.id)} onChange={() => flip(s.id)} />
                {byApproval && chosen.includes(s.id) && (
                  <>
                    <p className="settings-muted">{t('settings.integrations.approvalSpaceNote')}</p>
                    <Checkbox
                      label={t('settings.integrations.keepSpace', { name: spaceName(t, s) })}
                      checked={kept.includes(s.id)}
                      onChange={() => flipKept(s.id)}
                    />
                  </>
                )}
              </div>
            )
          })}
        </fieldset>
        <FormError error={action.error} />
      </form>
    </Dialog>
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
  const action = useAction(
    new Map([
      ['name', t('settings.actions.name')],
      ['display_name', t('settings.providers.displayName')],
      ['max_amount_minor', t('settings.integrations.ceiling')],
      ['currencies', t('settings.integrations.currencies')],
      ...registry.providers.flatMap((p) => [
        ...p.secrets.map((x) => [x.name, label(t, 'secret', x.name)] as const),
        ...p.settings.map((x) => [x.name, label(t, 'setting', x.name)] as const),
      ]),
    ]),
  )
  const formId = useId()
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
  const pickProvider = (e: { target: { value: string } }) => {
    set('provider')(e)
    setSettings({})
    setSecrets({})
  }
  const ceiling = Number(form.max_amount_minor)
  const preview =
    Number.isSafeInteger(ceiling) && ceiling > 0 && currencies.length > 0
      ? amounts(f, registry.exponents, ceiling, currencies)
      : null

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (!provider) return
    const typed = Object.fromEntries(
      provider.secrets
        .map((spec) => [spec.name, (secrets[spec.name] ?? '').trim()] as const)
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

  const name = integration
    ? t('settings.actions.editNamed', { name: integration.display_name })
    : t('settings.integrations.newIntegration')
  return (
    <Dialog
      open
      onClose={onCancel}
      dismissible={!action.busy}
      title={name}
      actions={
        <>
          <Button onClick={onCancel} disabled={action.busy}>
            {t('common.cancel')}
          </Button>
          <Button
            type="submit"
            form={formId}
            variant="primary"
            loading={action.busy}
            disabled={currencies.length === 0}
          >
            {integration ? t('settings.actions.save') : t('settings.actions.add')}
          </Button>
        </>
      }
    >
      <form id={formId} className="settings-form" onSubmit={submit} aria-label={name}>
        {!integration && (
          <>
            <Field label={t('settings.integrations.providerLabel')} required>
              <Select value={form.provider} onChange={pickProvider} data-autofocus>
                {registry.providers.map((p) => (
                  <option key={p.id} value={p.id}>
                    {label(t, 'provider', p.id)}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label={t('settings.actions.name')} hint={t('settings.providers.nameHint')} required>
              <Input
                value={form.name}
                onChange={set('name')}
                maxLength={40}
                pattern="[a-z0-9][a-z0-9._\-]*"
                autoComplete="off"
                spellCheck={false}
              />
            </Field>
          </>
        )}
        <Field label={t('settings.providers.displayName')}>
          <Input value={form.display_name} onChange={set('display_name')} maxLength={100} placeholder={form.name} />
        </Field>
        {provider?.settings.map((spec) => (
          <Field key={spec.name} label={label(t, 'setting', spec.name)} required={spec.required}>
            {spec.options.length > 0 ? (
              <Select
                value={settings[spec.name] ?? ''}
                onChange={(e) => setSettings((x) => ({ ...x, [spec.name]: e.target.value }))}
              >
                <option value="" />
                {spec.options.map((o) => (
                  <option key={o} value={o}>
                    {label(t, 'settingOption', o)}
                  </option>
                ))}
              </Select>
            ) : (
              <Input
                value={settings[spec.name] ?? ''}
                maxLength={64}
                autoComplete="off"
                spellCheck={false}
                onChange={(e) => setSettings((x) => ({ ...x, [spec.name]: e.target.value }))}
              />
            )}
          </Field>
        ))}
        {provider?.secrets.map((spec) => {
          const stored = integration?.secrets[spec.name] === 'set'
          const name = label(t, 'secret', spec.name)
          return (
            <Field
              key={spec.name}
              label={stored ? t('settings.integrations.newSecret', { name }) : name}
              hint={stored ? t('settings.integrations.secretHintStored') : t('settings.integrations.secretHint')}
              required={spec.required && !stored}
            >
              <Input
                type="password"
                value={secrets[spec.name] ?? ''}
                onChange={(e) => setSecrets((x) => ({ ...x, [spec.name]: e.target.value }))}
                autoComplete="new-password"
                spellCheck={false}
                placeholder={
                  stored
                    ? t('settings.integrations.keepSecret')
                    : spec.required
                      ? undefined
                      : t('settings.actions.optional')
                }
              />
            </Field>
          )
        })}
        <Field
          label={t('settings.integrations.ceiling')}
          hint={
            <>
              {t('settings.integrations.ceilingHint')}
              {preview && ` ${t('settings.integrations.ceilingPreview', { amounts: preview })}`}
            </>
          }
          required
        >
          <Input
            type="number"
            inputMode="numeric"
            min={1}
            max={registry.max_amount_minor}
            step={1}
            value={form.max_amount_minor}
            onChange={set('max_amount_minor')}
          />
        </Field>
        <fieldset className="settings-fieldset">
          <legend>
            {t('settings.integrations.currencies')}
            {' · '}
            {t('settings.integrations.currenciesChosen', { count: currencies.length })}
          </legend>
          <div className="settings-many-choices">
            {registry.currencies.map((c) => (
              <Checkbox
                key={c}
                label={c}
                checked={currencies.includes(c)}
                onChange={() => setCurrencies((cs) => (cs.includes(c) ? cs.filter((x) => x !== c) : [...cs, c]))}
              />
            ))}
          </div>
        </fieldset>
        <FormError error={action.error} />
      </form>
    </Dialog>
  )
}
