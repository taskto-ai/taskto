// A workspace's shared CLI accounts (docs/account-switching.md): API keys its admins add for
// Claude Code, Codex or Grok Build, which members' turns use through the server's proxy. The key
// never reaches the CLIs, and never comes back from the server. They are never subscription
// logins: the providers' terms forbid sharing one. Admins also set which accounts each member's
// turns use here (their own, the workspace's, or their own if connected, else the workspace's).

import { type FormEvent, useId, useState } from 'react'
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
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
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
import { CliLimits } from './CliAccountsTab'
import { Alert, Loading, Section, useAction } from './parts'

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
  const [confirmDialog, confirm] = useConfirm()
  const t = useT()
  const done = () => {
    setEditing(null)
    list.reload()
  }
  const add = (
    <Button icon="plus" onClick={() => setEditing('new')}>
      {t('settings.sharedCli.add')}
    </Button>
  )
  const empty = list.data?.accounts.length === 0
  const account = typeof editing === 'number' ? list.data?.accounts.find((a) => a.id === editing) : undefined
  return (
    <Section
      title={t('settings.sharedCli.title')}
      help={t('settings.sharedCli.intro')}
      actions={admin && !empty && add}
    >
      <ul className="settings-terms" aria-label={t('settings.sharedCli.termsLabel')}>
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
        <Notice tone="neutral">{codeText(list.data.reason_code, undefined, list.data.reason)}</Notice>
      )}
      {list.data && <Notice tone="info">{t(YOURS[list.data.mode])}</Notice>}
      {!list.data && !list.error && <Loading lines={2} />}
      {empty && (
        <Panel>
          <EmptyState title={t('settings.sharedCli.none')} actions={admin && add}>
            {admin && t('settings.sharedCli.noneHelp')}
          </EmptyState>
        </Panel>
      )}
      {list.data && list.data.accounts.length > 0 && (
        <Panel>
          <Rows label={t('settings.sharedCli.title')}>
            {list.data.accounts.map((a) => (
              <SharedRow
                key={a.id}
                base={base}
                account={a}
                admin={admin}
                onEdit={() => setEditing(a.id)}
                onRemoved={done}
                confirm={confirm}
              />
            ))}
          </Rows>
        </Panel>
      )}
      {(editing === 'new' || account) && (
        <SharedForm
          key={account?.id ?? 'new'}
          base={base}
          account={account}
          onDone={done}
          onCancel={() => setEditing(null)}
        />
      )}
      {admin && <Modes base={base} onChanged={list.reload} />}
      {confirmDialog}
    </Section>
  )
}

function SharedRow({
  base,
  account: a,
  admin,
  onEdit,
  onRemoved,
  confirm,
}: {
  base: string
  account: SharedCliAccount
  admin: boolean
  onEdit: () => void
  onRemoved: () => void
  confirm: (options: ConfirmOptions) => Promise<boolean>
}) {
  const [test, setTest] = useState<Test | null>(null)
  const action = useAction()
  const t = useT()
  const title = `${a.name} · ${a.label}`
  const run = () => {
    setTest(null)
    action.run(async () => setTest(await api<Test>(`${base}/${a.id}/test`, { method: 'POST', body: {} })))
  }
  const remove = async () => {
    const yes = await confirm({
      title: t('settings.sharedCli.removeQuestion', { name: title }),
      confirmLabel: t('settings.actions.removeConfirm'),
      tone: 'danger',
    })
    if (yes) action.run(() => api(`${base}/${a.id}`, { method: 'DELETE' })).then((ok) => ok && onRemoved())
  }
  return (
    <Row
      title={title}
      status={
        test &&
        (test.ok ? (
          <Badge tone="success" dot>
            {t('settings.sharedCli.accepted')}
          </Badge>
        ) : (
          <Badge tone="danger" dot>
            {t('settings.sharedCli.refused')}
          </Badge>
        ))
      }
      detail={
        <span className="settings-row-extra">
          <span>
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
            <span role={test.ok ? 'status' : 'alert'} className={test.ok ? undefined : 'settings-error-text'}>
              {test.ok ? t('settings.sharedCli.keyAccepted') : codeText(test.code, test.params, test.message)}
            </span>
          )}
          {action.error && (
            <span role="alert" className="settings-error-text">
              {action.error}
            </span>
          )}
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
        </span>
      }
      actions={
        admin && (
          <>
            <Button
              size="sm"
              aria-label={t('settings.actions.testNamed', { name: title })}
              loading={action.busy && !test}
              disabled={action.busy}
              onClick={run}
            >
              {t('settings.actions.test')}
            </Button>
            <MoreMenu label={t('settings.moreFor', { name: title })}>
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
  const formId = useId()
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
  const label = account
    ? t('settings.actions.editNamed', { name: `${account.name} · ${account.label}` })
    : t('settings.sharedCli.newAccount')
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
          <Field label={t('settings.sharedCli.cli')}>
            <Select value={form.cli} onChange={set('cli')}>
              {(Object.keys(CLI_PRODUCTS) as CliName[]).map((cli) => (
                <option key={cli} value={cli}>
                  {CLI_PRODUCTS[cli]}
                </option>
              ))}
            </Select>
          </Field>
        )}
        <Field label={t('settings.actions.name')}>
          <Input
            required
            value={form.label}
            onChange={set('label')}
            maxLength={60}
            placeholder={t('settings.sharedCli.namePlaceholder')}
          />
        </Field>
        <Field label={t('settings.sharedCli.address')} hint={t('settings.sharedCli.addressHint')}>
          <Input
            type="url"
            value={form.base_url}
            onChange={set('base_url')}
            placeholder={DEFAULT_URL[form.cli]}
            spellCheck={false}
          />
        </Field>
        <Field label={t('settings.actions.model')} hint={t('settings.sharedCli.modelHint')}>
          <Input
            value={form.model}
            onChange={set('model')}
            maxLength={128}
            spellCheck={false}
            placeholder={t('settings.actions.optional')}
          />
        </Field>
        <Field
          label={account ? t('settings.actions.newApiKey') : t('settings.actions.apiKey')}
          hint={t('settings.sharedCli.keyHint')}
        >
          <Input
            required={!account}
            type="password"
            value={form.api_key}
            onChange={set('api_key')}
            autoComplete="new-password"
            spellCheck={false}
            placeholder={account ? t('settings.actions.keepKey') : ''}
          />
        </Field>
        <Alert error={action.error} />
      </form>
    </Dialog>
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
    <Section title={t('settings.sharedCli.modesTitle')} level={3}>
      <Field label={t('settings.sharedCli.defaultMode')}>
        <Select
          value={modes.data.default_mode}
          disabled={action.busy}
          onChange={(e) => save({ default_mode: e.target.value })}
        >
          {(Object.keys(MODE_LABEL) as CliAccountMode[]).map((m) => (
            <option key={m} value={m}>
              {t(MODE_LABEL[m])}
            </option>
          ))}
        </Select>
      </Field>
      <Panel>
        <Rows>
          {modes.data.members.map((m) => (
            <Row
              key={m.user_id}
              title={m.name}
              detail={m.email}
              actions={
                <Select
                  size="sm"
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
                </Select>
              }
            />
          ))}
        </Rows>
      </Panel>
      <Alert error={action.error} />
    </Section>
  )
}
