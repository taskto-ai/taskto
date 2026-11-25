// Model provider accounts: the workspace's (its admins manage them, its members use them)
// and the viewer's own (only they see and use them, in any workspace). An API key goes in
// and never comes back: the API answers "set" or "not set", so the form only ever offers to
// replace or remove it.

import { type FormEvent, useId, useState } from 'react'
import { api, codeText, type Model, type ProviderAccount, type ProviderCheck, ws } from '../api'
import { useFormat, useT } from '../i18n'
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
import { Panel } from '../ui/Panel'
import { Row, Rows } from '../ui/Rows'
import { Textarea } from '../ui/Textarea'
import { Alert, Loading, Section, SettingsTab, useAction } from './parts'

export function ProvidersTab({ workspace, admin }: { workspace: number; admin: boolean }) {
  const t = useT()
  return (
    <SettingsTab title={t('settings.tabs.providers')} help={t('settings.help.providers')}>
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
    </SettingsTab>
  )
}

function Accounts({ title, base, editable, intro }: { title: string; base: string; editable: boolean; intro: string }) {
  const accounts = useLoad(() => api<{ providers: ProviderAccount[] }>(base).then((r) => r.providers), [base])
  const [editing, setEditing] = useState<number | 'new' | null>(null)
  const [confirmDialog, confirm] = useConfirm()
  const t = useT()
  const done = () => {
    setEditing(null)
    accounts.reload()
  }
  const add = (
    <Button icon="plus" onClick={() => setEditing('new')}>
      {t('settings.providers.add')}
    </Button>
  )
  const empty = accounts.data?.length === 0
  const account = typeof editing === 'number' ? accounts.data?.find((a) => a.id === editing) : undefined
  return (
    <Section title={title} help={intro} actions={editable && !empty && add}>
      <Alert error={accounts.error} />
      {!accounts.data && !accounts.error && <Loading lines={2} />}
      {empty && (
        <Panel>
          <EmptyState title={t('settings.providers.none')} actions={editable && add}>
            {editable && t('settings.providers.noneHelp')}
          </EmptyState>
        </Panel>
      )}
      {accounts.data && accounts.data.length > 0 && (
        <Panel>
          <Rows label={title}>
            {accounts.data.map((a) => (
              <AccountRow
                key={a.id}
                base={base}
                account={a}
                editable={editable}
                onEdit={() => setEditing(a.id)}
                onRemoved={done}
                confirm={confirm}
              />
            ))}
          </Rows>
        </Panel>
      )}
      {(editing === 'new' || account) && (
        <AccountForm
          key={account?.id ?? 'new'}
          base={base}
          account={account}
          onDone={done}
          onCancel={() => setEditing(null)}
        />
      )}
      {confirmDialog}
    </Section>
  )
}

function AccountRow({
  base,
  account: a,
  editable,
  onEdit,
  onRemoved,
  confirm,
}: {
  base: string
  account: ProviderAccount
  editable: boolean
  onEdit: () => void
  onRemoved: () => void
  confirm: Confirm
}) {
  const [check, setCheck] = useState<ProviderCheck | null>(null)
  const action = useAction()
  const t = useT()
  const f = useFormat()
  const test = () => {
    setCheck(null)
    action.run(async () => setCheck(await api<ProviderCheck>(`${base}/${a.id}/test`, { method: 'POST' })))
  }
  const remove = async () => {
    const yes = await confirm({
      title: t('settings.providers.removeQuestion', { name: a.display_name }),
      confirmLabel: t('settings.actions.removeConfirm'),
      tone: 'danger',
    })
    if (yes) action.run(() => api(`${base}/${a.id}`, { method: 'DELETE' })).then((ok) => ok && onRemoved())
  }

  return (
    <Row
      title={a.display_name}
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
          {a.api_key === 'set' ? (
            <Badge>{t('settings.providers.keySet')}</Badge>
          ) : (
            <Badge>{t('settings.providers.keyNotSet')}</Badge>
          )}
        </>
      }
      detail={
        <span className="settings-row-extra">
          <span>
            <code>{a.provider}</code>
            {' · '}
            <code>{a.base_url}</code>
          </span>
          <span>{f.list(a.models.map((m) => m.name || m.id))}</span>
          {action.busy && !check && <span>{t('settings.providers.testing')}</span>}
          {check?.ok && (
            <span role="status">
              {check.missing?.length
                ? t('settings.providers.answeredMissing', { models: f.list(check.missing) })
                : t('settings.providers.answeredAll')}
            </span>
          )}
          {check && !check.ok && (
            <span role="alert" className="settings-error-text">
              {codeText(check.code, check.params, check.error ?? '')}
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
        editable && (
          <>
            <Button
              size="sm"
              aria-label={t('settings.actions.testNamed', { name: a.display_name })}
              loading={action.busy && !check}
              disabled={action.busy}
              onClick={test}
            >
              {t('settings.actions.test')}
            </Button>
            <MoreMenu label={t('settings.moreFor', { name: a.display_name })}>
              <MenuItem icon="edit" onSelect={onEdit}>
                {t('settings.actions.edit')}
              </MenuItem>
              <MenuSeparator />
              <MenuItem tone="danger" onSelect={remove} disabled={action.busy}>
                {t('settings.actions.remove')}
              </MenuItem>
            </MoreMenu>
          </>
        )
      }
    />
  )
}

type Confirm = (options: ConfirmOptions) => Promise<boolean>

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
  const formId = useId()

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

  const label = account
    ? t('settings.actions.editNamed', { name: account.display_name })
    : t('settings.providers.newAccount')
  return (
    <Dialog
      open
      onClose={onCancel}
      title={label}
      actions={
        <>
          <Button onClick={onCancel}>{t('common.cancel')}</Button>
          <Button type="submit" form={formId} variant="primary" loading={action.busy}>
            {account ? t('settings.actions.save') : t('settings.actions.add')}
          </Button>
        </>
      }
    >
      <form id={formId} className="settings-form" onSubmit={submit} aria-label={label}>
        {!account && (
          <Field label={t('settings.actions.name')} hint={t('settings.providers.nameHint')} required>
            <Input
              value={form.name}
              onChange={set('name')}
              maxLength={40}
              pattern="[a-z0-9][a-z0-9._\-]*"
              autoComplete="off"
              spellCheck={false}
              data-autofocus
            />
          </Field>
        )}
        <Field label={t('settings.providers.displayName')}>
          <Input value={form.display_name} onChange={set('display_name')} maxLength={100} placeholder={form.name} />
        </Field>
        <Field label={t('settings.actions.baseUrl')} hint={t('settings.providers.urlHint')} required>
          <Input
            type="url"
            value={form.base_url}
            onChange={set('base_url')}
            placeholder="http://localhost:11434/v1"
            spellCheck={false}
          />
        </Field>
        <Field
          label={hasKey ? t('settings.actions.newApiKey') : t('settings.actions.apiKey')}
          hint={hasKey ? t('settings.providers.keyHintStored') : t('settings.providers.keyHint')}
        >
          <Input
            type="password"
            value={form.api_key}
            onChange={set('api_key')}
            autoComplete="new-password"
            spellCheck={false}
            placeholder={hasKey ? t('settings.actions.keepKey') : t('settings.actions.optional')}
          />
        </Field>
        {hasKey && (
          <Checkbox
            label={t('settings.providers.removeKey')}
            checked={form.remove_key}
            disabled={form.api_key.trim() !== ''}
            onChange={(e) => setForm((f) => ({ ...f, remove_key: e.target.checked }))}
          />
        )}
        <Field label={t('settings.providers.models')} hint={t('settings.providers.modelsHint')} required>
          <Textarea
            value={form.models}
            onChange={set('models')}
            rows={3}
            spellCheck={false}
            placeholder="llama3.1:8b"
          />
        </Field>
        <Alert error={action.error} />
      </form>
    </Dialog>
  )
}
