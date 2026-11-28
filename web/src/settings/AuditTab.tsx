import { useMemo, useRef, useState } from 'react'
import { type AuditEntry, type AuditPage, api, type Member, ws } from '../api'
import { type Format, has, type Key, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Button } from '../ui/Button'
import { EmptyState } from '../ui/EmptyState'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Panel } from '../ui/Panel'
import { Select } from '../ui/Select'
import { Table } from '../ui/Table'
import { Alert, FormActions, Loading, SettingsTab, useAction } from './parts'

export const AUDIT_ACTIONS = [
  'setup_completed',
  'login_succeeded',
  'login_failed',
  'logout',
  'session_revoked',
  'other_sessions_revoked',
  'password_changed',
  'password_change_failed',
  'password_reset_created',
  'password_reset_requested',
  'password_reset_used',
  'sso_login_succeeded',
  'sso_login_failed',
  'sso_identity_linked',
  'workspace_created',
  'workspace_updated',
  'invite_created',
  'invite_emailed',
  'invite_revoked',
  'invite_accepted',
  'member_role_changed',
  'member_removed',
  'member_disabled',
  'member_enabled',
  'account_deletion_requested',
  'account_deletion_cancelled',
  'account_purged',
  'access_token_created',
  'access_token_revoked',
  'access_token_used',
  'space_created',
  'space_updated',
  'space_member_added',
  'space_member_role_changed',
  'space_member_removed',
  'provider_account_created',
  'provider_account_updated',
  'provider_account_removed',
  'shared_cli_account_created',
  'shared_cli_account_updated',
  'shared_cli_account_removed',
  'cli_account_modes_changed',
  'search_embeddings_changed',
  'search_consent_changed',
  'dictation_settings_changed',
  'integration_created',
  'integration_updated',
  'integration_removed',
  'integration_disabled',
  'integration_enabled',
  'integration_spaces_changed',
  'integration_spaces_kept',
  'integration_new_link_allowed',
  'app_integration_requested',
  'app_integration_approved',
  'app_integration_rejected',
  'app_integration_revoked',
  'skill_created',
  'skill_updated',
  'skill_removed',
  'data_export_started',
  'workspace_export_started',
  'server_update_requested',
  'two_factor_enabled',
  'two_factor_disabled',
  'two_factor_recovery_codes_replaced',
  'two_factor_cleared',
] as const

export const AUDIT_TARGETS = [
  'user',
  'workspace',
  'invite',
  'space',
  'access_token',
  'provider_account',
  'shared_cli_account',
  'integration',
  'app_integration_approval',
  'skill',
] as const

export const AUDIT_DETAILS = [
  'access_grants',
  'account',
  'added',
  'allowed_by',
  'app_id',
  'already_member',
  'api_key_changed',
  'api_key_set',
  'archived',
  'base_host',
  'cascade',
  'channel',
  'cleanups_pending',
  'cli',
  'cli_accounts',
  'conversations',
  'created_account',
  'default_mode',
  'documents_anonymized',
  'effect_id',
  'email',
  'emailed',
  'enabled',
  'engine',
  'expires',
  'fields',
  'from',
  'integration_id',
  'kept',
  'kind',
  'language',
  'locale',
  'logins_ended',
  'members_changed',
  'members_signed_out',
  'mentions_anonymized',
  'method',
  'mode',
  'name',
  'notice_confirmed',
  'offline_downloads',
  'private_spaces',
  'provider',
  'purge_days',
  'push_subscriptions',
  'reason',
  'removed',
  'role',
  'scope',
  'space_allowed',
  'space_id',
  'spaces',
  'to',
  'uploads_removed',
  'user_id',
  'webhook_secret_changed',
  'webhook_secret_set',
] as const

const PAGE = 50

type Filters = { action: string; actor: string; target: string; from: string; to: string }
const NONE: Filters = { action: '', actor: '', target: '', from: '', to: '' }

function dayStart(day: string, plus = 0): string {
  const [y, m, d] = day.split('-').map(Number)
  const date = new Date(2000, 0, 1)
  date.setFullYear(y, m - 1, d + plus)
  return date.toISOString()
}

function queryOf(f: Filters): string {
  const q = new URLSearchParams({ limit: String(PAGE) })
  if (f.action) q.set('action', f.action)
  if (f.actor) q.set('actor', f.actor)
  if (f.target) q.set('target_type', f.target)
  if (f.from) q.set('from', dayStart(f.from))
  if (f.to) q.set('to', dayStart(f.to, 1))
  return q.toString()
}

export function AuditTab({ workspace }: { workspace: number }) {
  const t = useT()
  const base = ws(workspace)
  const [filters, setFilters] = useState<Filters>(NONE)
  const query = useMemo(() => queryOf(filters), [filters])
  const members = useLoad(() => api<{ members: Member[] }>(`${base}/members`).then((r) => r.members), [base])
  const first = useLoad(() => api<AuditPage>(`${base}/audit?${query}`), [base, query])
  const [older, setOlder] = useState<{ query: string; events: AuditEntry[]; next: string } | null>(null)
  const action = useAction()

  const shown = older && older.query === query ? older : null
  const events = [...(first.data?.events ?? []), ...(shown?.events ?? [])]
  const next = shown ? shown.next : (first.data?.next ?? '')

  const generation = useRef(0)
  const change = (patch: Partial<Filters>) => {
    generation.current++
    setOlder(null)
    setFilters((cur) => ({ ...cur, ...patch }))
  }
  const refresh = () => {
    generation.current++
    setOlder(null)
    first.reload()
  }
  const more = () =>
    action.run(async () => {
      const asked = generation.current
      const page = await api<AuditPage>(`${base}/audit?${query}&cursor=${encodeURIComponent(next)}`)
      if (asked !== generation.current) return
      setOlder({ query, events: [...(shown?.events ?? []), ...page.events], next: page.next })
    })

  const actions = useMemo(
    () => [...AUDIT_ACTIONS].sort((a, b) => t(actionKey(a)).localeCompare(t(actionKey(b)), t.locale)),
    [t],
  )
  const filtered = Object.values(filters).some(Boolean)

  return (
    <SettingsTab title={t('settings.audit.title')} help={t('settings.help.audit')}>
      <p className="settings-muted">{t('settings.audit.help')}</p>
      <Panel padded>
        <form
          className="settings-form"
          onSubmit={(e) => e.preventDefault()}
          aria-label={t('settings.audit.filters.title')}
        >
          <div className="settings-filters">
            <Field label={t('settings.audit.filters.action')}>
              <Select value={filters.action} onChange={(e) => change({ action: e.target.value })}>
                <option value="">{t('settings.audit.filters.any')}</option>
                {actions.map((a) => (
                  <option key={a} value={a}>
                    {t(actionKey(a))}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label={t('settings.audit.filters.actor')}>
              <Select value={filters.actor} onChange={(e) => change({ actor: e.target.value })}>
                <option value="">{t('settings.audit.filters.anyone')}</option>
                {(members.data ?? []).map((m) => (
                  <option key={m.id} value={m.id}>
                    {m.name}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label={t('settings.audit.filters.target')}>
              <Select value={filters.target} onChange={(e) => change({ target: e.target.value })}>
                <option value="">{t('settings.audit.filters.any')}</option>
                {AUDIT_TARGETS.map((x) => (
                  <option key={x} value={x}>
                    {t(targetKey(x))}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label={t('settings.audit.filters.from')}>
              <Input
                type="date"
                value={filters.from}
                max={filters.to || undefined}
                onChange={(e) => change({ from: e.target.value })}
              />
            </Field>
            <Field label={t('settings.audit.filters.to')}>
              <Input
                type="date"
                value={filters.to}
                min={filters.from || undefined}
                onChange={(e) => change({ to: e.target.value })}
              />
            </Field>
          </div>
          <FormActions>
            <Button variant="ghost" disabled={!filtered} onClick={() => change(NONE)}>
              {t('settings.audit.filters.clear')}
            </Button>
            <Button icon="clock" onClick={refresh}>
              {t('settings.audit.refresh')}
            </Button>
          </FormActions>
        </form>
      </Panel>
      <Alert error={first.error ?? members.error ?? action.error} />
      {!first.data && !first.error && <Loading lines={5} />}
      {first.data && events.length === 0 && (
        <EmptyState title={t(filtered ? 'settings.audit.noMatches' : 'settings.audit.empty')} />
      )}
      {events.length > 0 && (
        <Panel>
          <Table
            caption={t('settings.audit.title')}
            rows={events}
            rowKey={(e) => e.id}
            columns={[
              { key: 'event', header: t('settings.audit.event'), cell: (e) => <Event e={e} /> },
              { key: 'when', header: t('settings.audit.when'), cell: (e) => <When e={e} /> },
            ]}
          />
        </Panel>
      )}
      {next && (
        <FormActions>
          <Button loading={action.busy} onClick={more}>
            {t('settings.audit.more')}
          </Button>
        </FormActions>
      )}
    </SettingsTab>
  )
}

const actionKey = (a: string) => `settings.audit.actions.${a}` as Key
const targetKey = (x: string) => `settings.audit.targets.${x}` as Key
const detailKey = (d: string) => `settings.audit.details.${d}` as Key
const valueKey = (v: string) => `settings.audit.values.${v}` as Key

const CODED = new Set([
  'allowed_by',
  'role',
  'from',
  'to',
  'default_mode',
  'method',
  'scope',
  'reason',
  'fields',
  'mode',
])

function Event({ e }: { e: AuditEntry }) {
  const t = useT()
  const f = useFormat()
  const label = has(`settings.audit.actions.${e.action}`) ? t(actionKey(e.action)) : e.action
  return (
    <div className="settings-audit-event">
      <strong>{label}</strong>
      <span>{who(e, t)}</span>
      {e.target_id !== null || e.target_type ? <span className="settings-muted">{what(e, t)}</span> : null}
      <Details details={e.details} f={f} />
    </div>
  )
}

function When({ e }: { e: AuditEntry }) {
  const f = useFormat()
  return (
    <div className="settings-audit-when">
      <time dateTime={e.at}>{f.dateTime(e.at)}</time>
      {e.client_addr && <span>{e.client_addr}</span>}
    </div>
  )
}

type Translate = ReturnType<typeof useT>

function who(e: AuditEntry, t: Translate): string {
  if (e.actor) return t('settings.audit.by', { name: e.actor.name })
  if (e.actor_deleted) return t('settings.audit.by', { name: t('common.deletedUser') })
  if (e.actor_id !== null) return t('settings.audit.byFormer', { id: e.actor_id })
  return t('settings.audit.byNobody')
}

function what(e: AuditEntry, t: Translate): string {
  const type = has(`settings.audit.targets.${e.target_type}`) ? t(targetKey(e.target_type)) : e.target_type
  const name = e.target_user?.name ?? e.target_name ?? (e.target_deleted ? t('common.deletedUser') : null)
  if (name) return t('settings.audit.target', { type, name })
  if (e.target_id !== null) return t('settings.audit.targetGone', { type, id: e.target_id })
  return type
}

function Details({ details, f }: { details: AuditEntry['details']; f: Format }) {
  const t = useT()
  const names = Object.keys(details).sort()
  if (names.length === 0) return null
  const code = (name: string, v: string) => (CODED.has(name) && has(`settings.audit.values.${v}`) ? t(valueKey(v)) : v)
  const value = (name: string, v: AuditEntry['details'][string]): string => {
    if (typeof v === 'boolean') return t(v ? 'settings.audit.yes' : 'settings.audit.no')
    if (typeof v === 'number') return f.number(v)
    if (Array.isArray(v)) return f.list(v.map((x) => code(name, x)))
    return code(name, v)
  }
  return (
    <dl className="settings-audit-details">
      {names.map((n) => (
        <div key={n}>
          <dt>{has(`settings.audit.details.${n}`) ? t(detailKey(n)) : n}</dt>
          <dd>{value(n, details[n])}</dd>
        </div>
      ))}
    </dl>
  )
}
