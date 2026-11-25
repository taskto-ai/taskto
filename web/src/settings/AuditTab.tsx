// The audit log of the workspace, for its admins: who did what to which thing, when and from
// which address (docs/audit.md). Newest first, with filters and a button for older events. The
// names of people and things are the server's, read when the log is: a person who has left is
// shown by their number only.

import { useMemo, useRef, useState } from 'react'
import { type AuditEntry, type AuditPage, api, type Member, ws } from '../api'
import { type Format, has, type Key, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Alert, useAction } from './parts'

// Every action the server records (internal/audit/actions.go): a test checks that this list, the
// server's and both languages' labels agree.
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
  'invite_revoked',
  'invite_accepted',
  'member_role_changed',
  'member_removed',
  'member_disabled',
  'member_enabled',
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
  'data_export_started',
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
] as const

// The names of the facts an event may carry (internal/audit/schema.go).
export const AUDIT_DETAILS = [
  'account',
  'allowed_by',
  'already_member',
  'api_key_changed',
  'api_key_set',
  'archived',
  'base_host',
  'cascade',
  'cli',
  'created_account',
  'default_mode',
  'email',
  'emailed',
  'enabled',
  'engine',
  'expires',
  'fields',
  'from',
  'kind',
  'language',
  'locale',
  'logins_ended',
  'members_changed',
  'members_signed_out',
  'method',
  'notice_confirmed',
  'offline_downloads',
  'provider',
  'reason',
  'role',
  'scope',
  'to',
  'user_id',
] as const

const PAGE = 50

type Filters = { action: string; actor: string; target: string; from: string; to: string }
const NONE: Filters = { action: '', actor: '', target: '', from: '', to: '' }

// A date input's day, from midnight to midnight where the admin is: the server compares times.
function dayStart(day: string, plus = 0): string {
  const [y, m, d] = day.split('-').map(Number)
  // setFullYear, not the constructor: years 0 to 99 are 1900 to 1999 there.
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
  // Older pages the admin asked for, of one query: a new query or a refresh drops them.
  const [older, setOlder] = useState<{ query: string; events: AuditEntry[]; next: string } | null>(null)
  const action = useAction()

  const shown = older && older.query === query ? older : null
  const events = [...(first.data?.events ?? []), ...(shown?.events ?? [])]
  const next = shown ? shown.next : (first.data?.next ?? '')

  // Bumped when what is shown is replaced, so an older page that was still on its way is dropped.
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
    <div className="tt-settings">
      <section className="tt-panel" aria-labelledby="tt-audit">
        <h2 id="tt-audit">{t('settings.audit.title')}</h2>
        <p className="tt-muted">{t('settings.audit.help')}</p>
        <form className="tt-form tt-inline tt-audit-filters" onSubmit={(e) => e.preventDefault()}>
          <label>
            {t('settings.audit.filters.action')}
            <select value={filters.action} onChange={(e) => change({ action: e.target.value })}>
              <option value="">{t('settings.audit.filters.any')}</option>
              {actions.map((a) => (
                <option key={a} value={a}>
                  {t(actionKey(a))}
                </option>
              ))}
            </select>
          </label>
          <label>
            {t('settings.audit.filters.actor')}
            <select value={filters.actor} onChange={(e) => change({ actor: e.target.value })}>
              <option value="">{t('settings.audit.filters.anyone')}</option>
              {(members.data ?? []).map((m) => (
                <option key={m.id} value={m.id}>
                  {m.name}
                </option>
              ))}
            </select>
          </label>
          <label>
            {t('settings.audit.filters.target')}
            <select value={filters.target} onChange={(e) => change({ target: e.target.value })}>
              <option value="">{t('settings.audit.filters.any')}</option>
              {AUDIT_TARGETS.map((x) => (
                <option key={x} value={x}>
                  {t(targetKey(x))}
                </option>
              ))}
            </select>
          </label>
          <label>
            {t('settings.audit.filters.from')}
            <input
              type="date"
              value={filters.from}
              max={filters.to || undefined}
              onChange={(e) => change({ from: e.target.value })}
            />
          </label>
          <label>
            {t('settings.audit.filters.to')}
            <input
              type="date"
              value={filters.to}
              min={filters.from || undefined}
              onChange={(e) => change({ to: e.target.value })}
            />
          </label>
          <div className="tt-row-actions">
            <button type="button" className="tt-button" disabled={!filtered} onClick={() => change(NONE)}>
              {t('settings.audit.filters.clear')}
            </button>
            <button type="button" className="tt-button" onClick={refresh}>
              {t('settings.audit.refresh')}
            </button>
          </div>
        </form>
        <Alert error={first.error ?? members.error ?? action.error} />
        {!first.data && !first.error && <p className="tt-muted">{t('app.loading')}</p>}
        {first.data && events.length === 0 && (
          <p className="tt-muted">{t(filtered ? 'settings.audit.noMatches' : 'settings.audit.empty')}</p>
        )}
        {events.length > 0 && (
          <ul className="tt-rows tt-audit-events" aria-label={t('settings.audit.title')}>
            {events.map((e) => (
              <Event key={e.id} e={e} />
            ))}
          </ul>
        )}
        {next && (
          <button type="button" className="tt-button" disabled={action.busy} onClick={more}>
            {t('settings.audit.more')}
          </button>
        )}
      </section>
    </div>
  )
}

const actionKey = (a: string) => `settings.audit.actions.${a}` as Key
const targetKey = (x: string) => `settings.audit.targets.${x}` as Key
const detailKey = (d: string) => `settings.audit.details.${d}` as Key
const valueKey = (v: string) => `settings.audit.values.${v}` as Key

// The details whose values are codes with a translation (roles, modes, the names of fields); the
// rest are names and addresses, shown as they are.
const CODED = new Set(['allowed_by', 'role', 'from', 'to', 'default_mode', 'method', 'scope', 'reason', 'fields'])

function Event({ e }: { e: AuditEntry }) {
  const t = useT()
  const f = useFormat()
  const label = has(`settings.audit.actions.${e.action}`) ? t(actionKey(e.action)) : e.action
  return (
    <li className="tt-row">
      <div className="tt-row-main">
        <strong>{label}</strong>
        <span className="tt-small">{who(e, t)}</span>
        {e.target_id !== null || e.target_type ? <span className="tt-small tt-muted">{what(e, t)}</span> : null}
        <Details details={e.details} f={f} />
      </div>
      <div className="tt-audit-when tt-small tt-muted">
        <time dateTime={e.at}>{f.dateTime(e.at)}</time>
        {e.client_addr && <span>{e.client_addr}</span>}
      </div>
    </li>
  )
}

type Translate = ReturnType<typeof useT>

function who(e: AuditEntry, t: Translate): string {
  if (e.actor) return t('settings.audit.by', { name: e.actor.name })
  if (e.actor_id !== null) return t('settings.audit.byFormer', { id: e.actor_id })
  return t('settings.audit.byNobody')
}

function what(e: AuditEntry, t: Translate): string {
  const type = has(`settings.audit.targets.${e.target_type}`) ? t(targetKey(e.target_type)) : e.target_type
  const name = e.target_user?.name ?? e.target_name
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
    <dl className="tt-audit-details tt-small tt-muted">
      {names.map((n) => (
        <div key={n}>
          <dt>{has(`settings.audit.details.${n}`) ? t(detailKey(n)) : n}</dt>
          <dd>{value(n, details[n])}</dd>
        </div>
      ))}
    </dl>
  )
}
