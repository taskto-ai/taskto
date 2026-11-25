// The viewer's own CLI accounts (docs/cli-workers.md): their logins to Claude Code, Codex and
// Grok Build, for conversations that run on those CLIs. They are the user's, not a workspace's:
// only they see and use them, in any of their workspaces. A user may connect several accounts
// to a CLI (docs/account-switching.md), each a login of their own.
//
// A login goes through the CLI's own flow on the server's worker. This page shows what the CLI
// printed: its sign-in URL, as a link only when it is on the CLI's known sign-in host, and its
// one-time code; for Claude Code, the code its sign-in page shows is pasted back here. Every
// string from the server is shown as text, never as HTML.

import { type FormEvent, useEffect, useRef, useState } from 'react'
import {
  ApiError,
  api,
  CLI_ACCOUNTS,
  type CliAccount,
  type CliAccounts,
  type CliLimit,
  type CliLogin,
  type CliName,
  cliAccountTitle,
  codeText,
  errorText,
  MAX_CLI_ACCOUNTS,
} from '../api'
import { type Key, t as translate, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Badge } from '../ui/Badge'
import { Button } from '../ui/Button'
import { useConfirm } from '../ui/ConfirmDialog'
import { Field } from '../ui/Field'
import { Heading } from '../ui/Heading'
import type { IconName } from '../ui/Icon'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Panel } from '../ui/Panel'
import { Alert, CopyField, FormActions, Loading, Section, SettingsTab, useAction } from './parts'
import { SharedCliAccountsPanel } from './SharedCliAccounts'
import { SwitchingPanel } from './SwitchingPanel'

// How often a login in progress is read again.
export const LOGIN_POLL_MS = 2000
// After a failed read, the next waits twice as long, up to this; reading stops for good after
// LOGIN_POLL_LIMIT_MS, when the CLI's login has expired anyway (15 minutes).
const LOGIN_POLL_MAX_MS = 30_000
const LOGIN_POLL_LIMIT_MS = 16 * 60_000

// Each CLI's terms, linked next to Connect.
export const CLI_TERMS: Record<CliName, string> = {
  claude: 'https://code.claude.com/docs/en/legal-and-compliance',
  codex: 'https://openai.com/policies/terms-of-use/',
  grok: 'https://x.ai/legal/terms-of-service',
}

const STATE_LABEL: Record<CliAccount['state'], Key> = {
  connected: 'settings.cliAccounts.state.connected',
  reconnect: 'settings.cliAccounts.state.reconnect',
  disconnected: 'settings.cliAccounts.state.disconnected',
}

export function CliAccountsTab({ workspace, admin = false }: { workspace?: number; admin?: boolean } = {}) {
  const list = useLoad(() => api<CliAccounts>(CLI_ACCOUNTS), [])
  const t = useT()
  return (
    <SettingsTab title={t('settings.tabs.cliAccounts')} help={t('settings.help.cliAccounts')}>
      <Section title={t('settings.cliAccounts.title')} help={t('settings.cliAccounts.intro')}>
        <Alert error={list.error} />
        {!list.data && !list.error && <Loading />}
        {list.data && !list.data.enabled && (
          <Notice tone="neutral">
            {list.data.reason
              ? codeText(list.data.reason_code, undefined, list.data.reason)
              : t('settings.cliAccounts.off')}
          </Notice>
        )}
        {list.data?.enabled &&
          list.data.accounts.map((a, i, all) => (
            <div key={a.id} className="settings-cli-account">
              <AccountCard account={a} onChanged={list.reload} />
              {all[i + 1]?.cli !== a.cli && (
                <AddAccount accounts={all.filter((b) => b.cli === a.cli)} onChanged={list.reload} />
              )}
            </div>
          ))}
      </Section>
      {list.data?.enabled && <SwitchingPanel />}
      {workspace !== undefined && <SharedCliAccountsPanel workspace={workspace} admin={admin} />}
    </SettingsTab>
  )
}

// Another account for a CLI, on its next free number: offered once every account of the CLI
// listed is connected (or needs a new sign-in), so an unused one is filled first.
function AddAccount({ accounts, onChanged }: { accounts: CliAccount[]; onChanged: () => void }) {
  const t = useT()
  const first = accounts[0]
  if (!first?.available || accounts.some((a) => a.state === 'disconnected') || accounts.length >= MAX_CLI_ACCOUNTS) {
    return null
  }
  const used = new Set(accounts.map((a) => a.slot))
  let next = 2
  while (used.has(next)) next++
  if (next > MAX_CLI_ACCOUNTS) return null
  return (
    <div className="settings-cli-add">
      <CliLoginFlow
        account={`${first.cli}-${next}`}
        name={first.name}
        label={t('settings.cliAccounts.addAnother')}
        icon="plus"
        onConnected={onChanged}
      />
      <p className="settings-muted">{t('settings.cliAccounts.ownLogin')}</p>
    </div>
  )
}

const STATE_TONE: Record<CliAccount['state'], 'success' | 'warning' | 'neutral'> = {
  connected: 'success',
  reconnect: 'warning',
  disconnected: 'neutral',
}

function AccountCard({ account: a, onChanged }: { account: CliAccount; onChanged: () => void }) {
  const action = useAction()
  const [confirmDialog, confirm] = useConfirm()
  const t = useT()
  const check = () =>
    action.run(() => api(`${CLI_ACCOUNTS}/${a.id}/check`, { method: 'POST', body: {} })).then(onChanged)
  const title = cliAccountTitle(a)
  const disconnect = async () => {
    const yes = await confirm({
      title: t('settings.cliAccounts.disconnectQuestion', { name: title }),
      confirmLabel: t('settings.cliAccounts.disconnectConfirm'),
      tone: 'danger',
    })
    if (yes) action.run(() => api(`${CLI_ACCOUNTS}/${a.id}`, { method: 'DELETE' })).then((ok) => ok && onChanged())
  }

  return (
    <Panel as="section" padded className="settings-panel-body" aria-label={title}>
      <header className="settings-section-head">
        <div className="settings-section-titles">
          <Heading level={3} className="settings-person-name">
            {title}
            <Badge tone={STATE_TONE[a.state] ?? 'neutral'} dot={a.state === 'connected'}>
              {STATE_LABEL[a.state] ? t(STATE_LABEL[a.state]) : a.state}
            </Badge>
          </Heading>
          {a.auth_method && (
            <p className="settings-section-help">{t('settings.cliAccounts.signedInWith', { method: a.auth_method })}</p>
          )}
        </div>
        {a.available && (
          <div className="settings-section-actions">
            <Button
              size="sm"
              aria-label={t('settings.cliAccounts.checkNamed', { name: title })}
              loading={action.busy}
              onClick={check}
            >
              {t('settings.cliAccounts.check')}
            </Button>
            {a.state !== 'disconnected' && (
              <Button
                size="sm"
                variant="ghost"
                aria-label={t('common.labelFor', { label: t('settings.cliAccounts.disconnect'), name: title })}
                disabled={action.busy}
                onClick={disconnect}
              >
                {t('settings.cliAccounts.disconnect')}
              </Button>
            )}
          </div>
        )}
      </header>
      {!a.available && <Notice tone="neutral">{t('settings.cliAccounts.notInstalled')}</Notice>}
      <Alert error={action.error} />
      {a.state !== 'disconnected' && <LabelForm account={a} onChanged={onChanged} />}
      {a.available && (
        <>
          <CliLoginFlow
            account={a.id}
            name={title}
            offer={a.state !== 'connected'}
            label={a.state === 'reconnect' ? t('settings.cliAccounts.reconnect') : t('settings.cliAccounts.connect')}
            initial={a.login}
            onConnected={onChanged}
          />
          <p className="settings-muted">
            {t('settings.cliAccounts.ownSubscription')}{' '}
            <a href={CLI_TERMS[a.cli]} target="_blank" rel="noopener noreferrer">
              {t('settings.actions.termsOf', { name: a.name })}
            </a>
          </p>
        </>
      )}
      <CliLimits account={a} />
      {confirmDialog}
    </Panel>
  )
}

// The user's name for an account, to tell several apart.
function LabelForm({ account: a, onChanged }: { account: CliAccount; onChanged: () => void }) {
  const [label, setLabel] = useState(a.label)
  const action = useAction()
  const t = useT()
  // The draft follows the account's name when the server's changes. This is not an effect: an
  // effect runs after the first commit, and a name typed before it ran (the commit and the typing
  // are both quick) was overwritten with the saved one.
  const [saved, setSaved] = useState(a.label)
  if (saved !== a.label) {
    setSaved(a.label)
    setLabel(a.label)
  }
  const save = (e: FormEvent) => {
    e.preventDefault()
    action
      .run(() => api(`${CLI_ACCOUNTS}/${a.id}`, { method: 'PATCH', body: { label: label.trim() } }))
      .then((ok) => ok && onChanged())
  }
  return (
    <form
      className="settings-form"
      onSubmit={save}
      aria-label={t('settings.cliAccounts.nameOf', { name: cliAccountTitle(a) })}
    >
      <div className="settings-form-row">
        <Field label={t('settings.actions.name')}>
          <Input
            value={label}
            onChange={(e) => setLabel(e.target.value)}
            maxLength={40}
            placeholder={t('settings.cliAccounts.namePlaceholder')}
          />
        </Field>
        <Button type="submit" loading={action.busy} disabled={label.trim() === a.label}>
          {t('settings.actions.save')}
        </Button>
      </div>
      <Alert error={action.error} />
    </form>
  )
}

const live = (l: CliLogin | null | undefined) => l?.state === 'starting' || l?.state === 'waiting'

// A login shown on its own: one in progress, or one that ended without connecting (with why).
const shown = (l: CliLogin | null | undefined) =>
  l && (live(l) || l.state === 'failed' || l.state === 'expired') ? l : null

// Connects a CLI through its own login flow: starts the login (or picks up the one in
// progress), shows its URL and code, takes Claude Code's pasted code, and reads it again every
// LOGIN_POLL_MS until it connects, fails or expires. `offer` shows the button that starts one.
export function CliLoginFlow({
  account,
  name,
  label,
  offer = true,
  initial,
  icon,
  onConnected,
}: {
  // The account's handle: `claude`, or `claude-2` for another account.
  account: string
  name: string
  label: string
  offer?: boolean
  initial?: CliLogin | null
  icon?: IconName
  onConnected: () => void
}) {
  const [login, setLogin] = useState<CliLogin | null>(() => shown(initial))
  const [connected, setConnected] = useState(false)
  const action = useAction()
  const t = useT()
  const base = `${CLI_ACCOUNTS}/${account}/login`
  const done = useRef(onConnected)
  done.current = onConnected

  const update = (next: CliLogin) => {
    setLogin(next)
    if (next.state === 'connected') {
      setConnected(true)
      setLogin(null)
      done.current()
    } else if (next.state === 'cancelled') {
      setLogin(null)
    }
  }
  const start = () => {
    setConnected(false)
    setPollError(null)
    action.run(async () => update(await api<CliLogin>(base, { method: 'POST', body: {} })))
  }
  const cancel = () =>
    action
      .run(() => api(base, { method: 'DELETE' }))
      .then((ok) => {
        if (!ok) return
        setPollError(null)
        setLogin(null)
      })

  // Reads the login again while it is in progress, one read at a time: each read that ends
  // (answered or failed) moves tick on, which schedules the next one; a login that ends stops
  // it. A login the server no longer has ended there (cancelled elsewhere, or the server
  // restarted): it is dropped, and can be started again.
  const polling = live(login)
  const [pollError, setPollError] = useState<string | null>(null)
  const [tick, setTick] = useState(0)
  const failures = useRef(0)
  const pollingSince = useRef(0)
  const updateRef = useRef(update)
  updateRef.current = update
  useEffect(() => {
    if (!polling) {
      pollingSince.current = 0
      return
    }
    void tick // each read that ends moves tick on, which runs this again for the next one
    pollingSince.current ||= Date.now()
    if (Date.now() - pollingSince.current > LOGIN_POLL_LIMIT_MS) {
      setPollError(translate('settings.cliAccounts.tookTooLong'))
      return
    }
    let stopped = false
    const delay = Math.min(LOGIN_POLL_MS * 2 ** failures.current, LOGIN_POLL_MAX_MS)
    const timer = setTimeout(async () => {
      try {
        const next = await api<CliLogin>(base)
        if (stopped) return
        failures.current = 0
        setPollError(null)
        updateRef.current(next)
      } catch (e) {
        if (stopped) return
        if (e instanceof ApiError && e.status === 404) {
          setLogin(null)
          setPollError(translate('settings.cliAccounts.endedOnServer'))
          return
        }
        failures.current++
        setPollError(errorText(e)) // tried again, a little later each time; the error shows meanwhile
      }
      if (!stopped) setTick((n) => n + 1)
    }, delay)
    return () => {
      stopped = true
      clearTimeout(timer)
    }
  }, [polling, tick, base])

  if (!login) {
    return (
      <div className="settings-panel-body">
        {connected && !offer && <Notice tone="success">{t('settings.cliAccounts.signedIn', { name })}</Notice>}
        <Alert error={action.error ?? pollError} />
        {offer && (
          <FormActions>
            <Button
              icon={icon}
              aria-label={t('common.labelFor', { label, name })}
              loading={action.busy}
              onClick={start}
            >
              {label}
            </Button>
          </FormActions>
        )}
      </div>
    )
  }
  if (!live(login)) {
    return (
      <div className="settings-panel-body">
        <Notice tone="danger">
          {login.state === 'expired' ? t('settings.cliAccounts.expired') : t('settings.cliAccounts.failed')}
          {/* Why it failed, in the CLI's own words, which can't be translated; an expired one says no more. */}
          {login.state === 'failed' && login.message ? ` ${login.message}` : ''}
        </Notice>
        <Alert error={action.error} />
        <FormActions>
          <Button loading={action.busy} onClick={start}>
            {t('app.tryAgain')}
          </Button>
        </FormActions>
      </div>
    )
  }
  return (
    <Panel
      as="section"
      padded
      className="settings-panel-body settings-cli-login"
      aria-label={t('settings.cliAccounts.signingIn', { name })}
    >
      {login.state === 'starting' || !login.url ? (
        <p className="settings-muted">{t('settings.cliAccounts.starting', { name })}</p>
      ) : (
        <SignIn login={login} name={name} />
      )}
      {login.state === 'waiting' && login.needs_code && login.url && (
        <CodeForm base={base} name={name} onSent={update} />
      )}
      {login.state === 'waiting' && !login.needs_code && login.url && (
        <p className="settings-muted">{t('settings.cliAccounts.waiting')}</p>
      )}
      <Alert error={action.error ?? pollError} />
      <FormActions>
        <Button variant="ghost" disabled={action.busy} onClick={cancel}>
          {t('common.cancel')}
        </Button>
      </FormActions>
    </Panel>
  )
}

// The sign-in URL, a link only when it is on the CLI's known sign-in host (and https), and the
// one-time code to enter there.
function SignIn({ login, name }: { login: CliLogin; name: string }) {
  const t = useT()
  const url = login.url ?? ''
  const trusted = login.url_trusted && url.startsWith('https://')
  const message: Key = login.needs_code
    ? login.code
      ? 'settings.cliAccounts.signInAtWithCode'
      : 'settings.cliAccounts.signInAt'
    : login.code
      ? 'settings.cliAccounts.openWithCode'
      : 'settings.cliAccounts.open'
  return (
    <>
      {trusted ? (
        <p className="settings-text">
          {t.rich(message, {
            url,
            link: (text) => (
              <a href={url} target="_blank" rel="noopener noreferrer">
                {text}
              </a>
            ),
          })}
        </p>
      ) : (
        <>
          {/* A security warning: said at once, as an alert, in the warning's colour. */}
          <div role="alert">
            <Notice tone="warning" live={false}>
              {t('settings.cliAccounts.untrusted', { name })}
            </Notice>
          </div>
          <p className="settings-text">
            <code>{url}</code>
          </p>
          {login.code && <p className="settings-text">{t('settings.cliAccounts.enterCodeThere')}</p>}
        </>
      )}
      {login.code && (
        <CopyField
          value={login.code}
          label={t('settings.cliAccounts.oneTimeCode')}
          name={t('settings.cliAccounts.theCode')}
        />
      )}
    </>
  )
}

function CodeForm({ base, name, onSent }: { base: string; name: string; onSent: (l: CliLogin) => void }) {
  const [code, setCode] = useState('')
  const action = useAction()
  const t = useT()
  const submit = (e: FormEvent) => {
    e.preventDefault()
    const value = code.trim()
    if (!value) return
    action.run(async () => {
      onSent(await api<CliLogin>(`${base}/code`, { method: 'POST', body: { code: value } }))
      setCode('')
    })
  }
  return (
    <form className="settings-form" onSubmit={submit} aria-label={t('settings.cliAccounts.codeOf', { name })}>
      <div className="settings-form-row">
        <Field label={t('settings.cliAccounts.pasteCode')} required>
          <Input
            value={code}
            onChange={(e) => setCode(e.target.value)}
            autoComplete="off"
            spellCheck={false}
            maxLength={512}
          />
        </Field>
        <Button type="submit" loading={action.busy} disabled={!code.trim()}>
          {t('settings.cliAccounts.sendCode')}
        </Button>
      </div>
      <Alert error={action.error} />
    </form>
  )
}

// A window's name: Claude Code names its windows (five_hour, seven_day, …), Codex gives
// primary and secondary with their length in minutes.
const WINDOWS: Record<string, Key> = {
  five_hour: 'settings.cliAccounts.limits.fiveHour',
  seven_day: 'settings.cliAccounts.limits.weekly',
  seven_day_opus: 'settings.cliAccounts.limits.weeklyOpus',
  seven_day_sonnet: 'settings.cliAccounts.limits.weeklySonnet',
}

// A window of m minutes, by the largest unit that divides it: "5-hour limit".
function minutes(m: number): string {
  if (m % 10080 === 0) return translate('settings.cliAccounts.limits.weeks', { count: m / 10080 })
  if (m % 1440 === 0) return translate('settings.cliAccounts.limits.days', { count: m / 1440 })
  if (m % 60 === 0) return translate('settings.cliAccounts.limits.hours', { count: m / 60 })
  return translate('settings.cliAccounts.limits.minutes', { count: m })
}

// A limit's name in the current language. A window the app doesn't know shows by the name
// the CLI gives it.
export function windowLabel(l: CliLimit): string {
  if (WINDOWS[l.window]) return translate(WINDOWS[l.window])
  if (l.window_minutes && l.window_minutes > 0) return minutes(l.window_minutes)
  const words = l.window.replace(/_/g, ' ').trim()
  return words ? words[0].toUpperCase() + words.slice(1) : translate('settings.cliAccounts.limits.limit')
}

// The account's usage limits as the CLI last reported them, "as of" when that was.
export function CliLimits({ account: a }: { account: CliAccount }) {
  const t = useT()
  const f = useFormat()
  const when = (time: string) => (Number.isNaN(new Date(time).getTime()) ? time : f.dateTime(time))
  if (a.cli === 'grok') return <p className="settings-muted">{t('settings.cliAccounts.limits.grokNone')}</p>
  if (a.limits.length === 0) return <p className="settings-muted">{t('settings.cliAccounts.limits.none')}</p>
  return (
    <div className="settings-limits">
      <Heading level={4} size="subsection">
        {t('settings.cliAccounts.limits.title')}
      </Heading>
      <ul className="settings-limits-list">
        {a.limits.map((l) => {
          const label = windowLabel(l)
          const used = typeof l.used_percent === 'number' ? Math.min(100, Math.max(0, l.used_percent)) : null
          return (
            <li key={l.window} aria-label={label} className="settings-limit">
              <span className="settings-person-name">
                {label}
                {l.status === 'allowed_warning' && (
                  <Badge tone="warning">{t('settings.cliAccounts.limits.close')}</Badge>
                )}
                {l.status === 'rejected' && <Badge tone="danger">{t('settings.cliAccounts.limits.reached')}</Badge>}
              </span>
              {used !== null && (
                <span className="settings-meter">
                  <meter
                    min={0}
                    max={100}
                    low={70}
                    high={90}
                    optimum={0}
                    value={used}
                    aria-label={t('settings.cliAccounts.limits.usedNamed', { name: label })}
                  />
                  <span>
                    {t('settings.cliAccounts.limits.used', {
                      percent: f.number(Math.round(used) / 100, { style: 'percent' }),
                    })}
                  </span>
                </span>
              )}
              {l.resets_at && (
                <span className="settings-muted">
                  {t('settings.cliAccounts.limits.resets', { time: when(l.resets_at) })}
                </span>
              )}
              {l.detail && <span className="settings-muted">{l.detail}</span>}
            </li>
          )
        })}
      </ul>
      {a.limits_at && (
        <p className="settings-muted">{t('settings.cliAccounts.limits.asOf', { time: when(a.limits_at) })}</p>
      )}
    </div>
  )
}
