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
import { Alert, ConfirmButton, CopyButton, useAction } from './parts'
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
    <div className="tt-settings">
      <section className="tt-panel" aria-label={t('settings.tabs.cliAccounts')}>
        <div className="tt-pane-head">
          <h2>{t('settings.cliAccounts.title')}</h2>
        </div>
        <p className="tt-muted tt-small">{t('settings.cliAccounts.intro')}</p>
        <Alert error={list.error} />
        {!list.data && !list.error && <p className="tt-muted">{t('app.loading')}</p>}
        {list.data && !list.data.enabled && (
          <p className="tt-notice">
            {list.data.reason
              ? codeText(list.data.reason_code, undefined, list.data.reason)
              : t('settings.cliAccounts.off')}
          </p>
        )}
      </section>
      {list.data?.enabled &&
        list.data.accounts.map((a, i, all) => (
          <div key={a.id} className="tt-cli-accounts">
            <AccountCard account={a} onChanged={list.reload} />
            {all[i + 1]?.cli !== a.cli && (
              <AddAccount accounts={all.filter((b) => b.cli === a.cli)} onChanged={list.reload} />
            )}
          </div>
        ))}
      {list.data?.enabled && <SwitchingPanel />}
      {workspace !== undefined && <SharedCliAccountsPanel workspace={workspace} admin={admin} />}
    </div>
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
    <div className="tt-cli-add">
      <CliLoginFlow
        account={`${first.cli}-${next}`}
        name={first.name}
        label={t('settings.cliAccounts.addAnother')}
        onConnected={onChanged}
      />
      <p className="tt-muted tt-small">{t('settings.cliAccounts.ownLogin')}</p>
    </div>
  )
}

function AccountCard({ account: a, onChanged }: { account: CliAccount; onChanged: () => void }) {
  const action = useAction()
  const t = useT()
  const check = () =>
    action.run(() => api(`${CLI_ACCOUNTS}/${a.id}/check`, { method: 'POST', body: {} })).then(onChanged)
  const disconnect = () =>
    action.run(() => api(`${CLI_ACCOUNTS}/${a.id}`, { method: 'DELETE' })).then((ok) => ok && onChanged())
  const title = cliAccountTitle(a)
  const stateClass = a.state === 'connected' ? 'tt-ok' : a.state === 'reconnect' ? 'tt-warning' : 'tt-muted'

  return (
    <section className="tt-panel tt-cli-account" aria-label={title}>
      <div className="tt-pane-head">
        <h2>{title}</h2>
        {a.available && (
          <div className="tt-row-actions">
            <button
              type="button"
              className="tt-button"
              aria-label={t('settings.cliAccounts.checkNamed', { name: title })}
              disabled={action.busy}
              onClick={check}
            >
              {t('settings.cliAccounts.check')}
            </button>
            {a.state !== 'disconnected' && (
              <ConfirmButton
                label={t('settings.cliAccounts.disconnect')}
                name={title}
                question={t('settings.cliAccounts.disconnectQuestion', { name: title })}
                confirm={t('settings.cliAccounts.disconnectConfirm')}
                disabled={action.busy}
                onConfirm={disconnect}
              />
            )}
          </div>
        )}
      </div>
      <p className="tt-small">
        <span className={stateClass}>{STATE_LABEL[a.state] ? t(STATE_LABEL[a.state]) : a.state}</span>
        {a.auth_method && (
          <span className="tt-muted">
            {' · '}
            {t('settings.cliAccounts.signedInWith', { method: a.auth_method })}
          </span>
        )}
      </p>
      {!a.available && <p className="tt-muted">{t('settings.cliAccounts.notInstalled')}</p>}
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
          <p className="tt-muted tt-small">
            {t('settings.cliAccounts.ownSubscription')}{' '}
            <a href={CLI_TERMS[a.cli]} target="_blank" rel="noopener noreferrer">
              {t('settings.actions.termsOf', { name: a.name })}
            </a>
          </p>
        </>
      )}
      <CliLimits account={a} />
    </section>
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
      className="tt-form tt-inline"
      onSubmit={save}
      aria-label={t('settings.cliAccounts.nameOf', { name: cliAccountTitle(a) })}
    >
      <label>
        {t('settings.actions.name')}
        <input
          value={label}
          onChange={(e) => setLabel(e.target.value)}
          maxLength={40}
          placeholder={t('settings.cliAccounts.namePlaceholder')}
        />
      </label>
      <button type="submit" className="tt-button" disabled={action.busy || label.trim() === a.label}>
        {t('settings.actions.save')}
      </button>
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
  onConnected,
}: {
  // The account's handle: `claude`, or `claude-2` for another account.
  account: string
  name: string
  label: string
  offer?: boolean
  initial?: CliLogin | null
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
      <div className="tt-cli-login">
        {connected && !offer && (
          <p className="tt-ok tt-small" role="status">
            {t('settings.cliAccounts.signedIn', { name })}
          </p>
        )}
        <Alert error={action.error ?? pollError} />
        {offer && (
          <div>
            <button
              type="button"
              className="tt-button tt-primary"
              aria-label={t('common.labelFor', { label, name })}
              disabled={action.busy}
              onClick={start}
            >
              {label}
            </button>
          </div>
        )}
      </div>
    )
  }
  if (!live(login)) {
    return (
      <div className="tt-cli-login">
        <p role="alert" className="tt-error">
          {login.state === 'expired' ? t('settings.cliAccounts.expired') : t('settings.cliAccounts.failed')}
          {/* Why it failed, in the CLI's own words, which can't be translated; an expired one says no more. */}
          {login.state === 'failed' && login.message ? ` ${login.message}` : ''}
        </p>
        <Alert error={action.error} />
        <div>
          <button type="button" className="tt-button" disabled={action.busy} onClick={start}>
            {t('app.tryAgain')}
          </button>
        </div>
      </div>
    )
  }
  return (
    <section className="tt-notice tt-cli-login" aria-label={t('settings.cliAccounts.signingIn', { name })}>
      {login.state === 'starting' || !login.url ? (
        <p className="tt-muted">{t('settings.cliAccounts.starting', { name })}</p>
      ) : (
        <SignIn login={login} name={name} />
      )}
      {login.state === 'waiting' && login.needs_code && login.url && (
        <CodeForm base={base} name={name} onSent={update} />
      )}
      {login.state === 'waiting' && !login.needs_code && login.url && (
        <p className="tt-muted tt-small">{t('settings.cliAccounts.waiting')}</p>
      )}
      <Alert error={action.error ?? pollError} />
      <div>
        <button type="button" className="tt-button" disabled={action.busy} onClick={cancel}>
          {t('common.cancel')}
        </button>
      </div>
    </section>
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
        <p>
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
          <p role="alert" className="tt-warning">
            {t('settings.cliAccounts.untrusted', { name })}
          </p>
          <p>
            <code className="tt-cli-url">{url}</code>
          </p>
          {login.code && <p>{t('settings.cliAccounts.enterCodeThere')}</p>}
        </>
      )}
      {login.code && (
        <div className="tt-copy">
          <input
            readOnly
            value={login.code}
            aria-label={t('settings.cliAccounts.oneTimeCode')}
            onFocus={(e) => e.target.select()}
          />
          <CopyButton text={login.code} name={t('settings.cliAccounts.theCode')} />
        </div>
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
    <form className="tt-form" onSubmit={submit} aria-label={t('settings.cliAccounts.codeOf', { name })}>
      <label>
        {t('settings.cliAccounts.pasteCode')}
        <input
          value={code}
          onChange={(e) => setCode(e.target.value)}
          autoComplete="off"
          spellCheck={false}
          maxLength={512}
          required
        />
      </label>
      <Alert error={action.error} />
      <div>
        <button type="submit" className="tt-button tt-primary" disabled={action.busy || !code.trim()}>
          {t('settings.cliAccounts.sendCode')}
        </button>
      </div>
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
  if (a.cli === 'grok') return <p className="tt-muted tt-small">{t('settings.cliAccounts.limits.grokNone')}</p>
  if (a.limits.length === 0) return <p className="tt-muted tt-small">{t('settings.cliAccounts.limits.none')}</p>
  return (
    <div className="tt-cli-limits">
      <h3>{t('settings.cliAccounts.limits.title')}</h3>
      <ul className="tt-rows">
        {a.limits.map((l) => {
          const label = windowLabel(l)
          const used = typeof l.used_percent === 'number' ? Math.min(100, Math.max(0, l.used_percent)) : null
          return (
            <li key={l.window} className="tt-row tt-cli-limit" aria-label={label} data-status={l.status || undefined}>
              <div className="tt-row-main">
                <strong>{label}</strong>
                {used !== null && (
                  <span className="tt-cli-meter">
                    <meter
                      min={0}
                      max={100}
                      low={70}
                      high={90}
                      optimum={0}
                      value={used}
                      aria-label={t('settings.cliAccounts.limits.usedNamed', { name: label })}
                    />
                    <span className="tt-small">
                      {t('settings.cliAccounts.limits.used', {
                        percent: f.number(Math.round(used) / 100, { style: 'percent' }),
                      })}
                    </span>
                  </span>
                )}
                {l.status === 'allowed_warning' && (
                  <span className="tt-warning tt-small">{t('settings.cliAccounts.limits.close')}</span>
                )}
                {l.status === 'rejected' && (
                  <span className="tt-error tt-small">{t('settings.cliAccounts.limits.reached')}</span>
                )}
                {l.resets_at && (
                  <span className="tt-muted tt-small">
                    {t('settings.cliAccounts.limits.resets', { time: when(l.resets_at) })}
                  </span>
                )}
                {l.detail && <span className="tt-small">{l.detail}</span>}
              </div>
            </li>
          )
        })}
      </ul>
      {a.limits_at && (
        <p className="tt-muted tt-small">{t('settings.cliAccounts.limits.asOf', { time: when(a.limits_at) })}</p>
      )}
    </div>
  )
}
