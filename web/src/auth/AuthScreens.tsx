// The screens before the app: first-run setup, logging in, invites and password reset links.
// Each answers with the GET /api/me body, which the caller takes as the session.

import { type FormEvent, type ReactNode, useEffect, useRef, useState } from 'react'
import { ApiError, api, codeText, errorText, type Me } from '../api'
import { BrandLogo } from '../BrandLogo'
import { t, useT } from '../i18n'

// The product's name, the same in every language.
const BRAND = 'TaskTo.AI'

function AuthFrame({ title, intro, children }: { title: string; intro?: ReactNode; children: ReactNode }) {
  return (
    <main className="tt-auth">
      <div className="tt-auth-card">
        <p className="tt-brand">
          <BrandLogo name={BRAND} />
        </p>
        <h1>{title}</h1>
        {intro}
        {children}
      </div>
    </main>
  )
}

// Where the provider's hosted payment page sends the payer back to: the server's own pages, which
// never come from an app's definition. The payer is a customer of whoever sent the link, often with
// no account here, so these pages call nothing and show nothing of the payment: the provider told
// them already, and what happened to the record is the business's to say.
export function PaymentReturnScreen({ outcome }: { outcome: 'done' | 'cancelled' }) {
  const t = useT()
  return (
    <AuthFrame title={outcome === 'done' ? t('payment.done.title') : t('payment.cancelled.title')}>
      <p>{outcome === 'done' ? t('payment.done.body') : t('payment.cancelled.body')}</p>
    </AuthFrame>
  )
}

// Until the server says how people sign in, nothing is shown that it might take back: no heading
// or form yet, so a screen is never drawn twice.
function AuthLoading() {
  const t = useT()
  return <p className="tt-center tt-muted">{t('app.loading')}</p>
}

// What a password step (a login, a reset link) answers for an account with two-factor login on:
// no session, a challenge for POST /api/login/two-factor.
type Challenged = { two_factor_required: true; challenge: string }
const needsCode = (reply: Me | Challenged): reply is Challenged => 'two_factor_required' in reply

// send may answer null: the step is done but nobody is logged in yet (a login that asks for a code).
function useSubmit(send: () => Promise<Me | null>, onDone: (me: Me) => void) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const me = await send()
      if (me) onDone(me)
      else setBusy(false)
    } catch (err) {
      setError(errorText(err))
      setBusy(false)
    }
  }
  return { busy, error, submit }
}

// How people can sign in on this server (GET /api/login/methods). A server that doesn't know the
// route (404) is an older one: passwords only. Any other failure, or an answer that isn't the
// shape the route promises, is an error to retry: guessing "passwords only" would show a form
// that a passwords-off server refuses, or hide the providers.
type LoginMethods = { password: boolean; sso: { id: string; name: string }[]; resetByEmail: boolean }
type MethodsAnswer = { password: boolean; sso: LoginMethods['sso']; reset_by_email?: boolean }

type MethodsState = { kind: 'loading' } | { kind: 'ready'; methods: LoginMethods } | { kind: 'error' }

function validMethods(v: unknown): v is MethodsAnswer {
  if (typeof v !== 'object' || v === null) return false
  const m = v as { password?: unknown; sso?: unknown; reset_by_email?: unknown }
  return (
    typeof m.password === 'boolean' &&
    // Absent on a server from before the emailed reset link: it has none.
    (m.reset_by_email === undefined || typeof m.reset_by_email === 'boolean') &&
    Array.isArray(m.sso) &&
    m.sso.every(
      (p) =>
        typeof p === 'object' &&
        p !== null &&
        typeof (p as { id?: unknown }).id === 'string' &&
        typeof (p as { name?: unknown }).name === 'string',
    )
  )
}

function useLoginMethods(): { state: MethodsState; retry: () => void } {
  const [state, setState] = useState<MethodsState>({ kind: 'loading' })
  const [attempt, setAttempt] = useState(0)
  // biome-ignore lint/correctness/useExhaustiveDependencies: attempt makes a Retry fetch again
  useEffect(() => {
    let live = true
    api<unknown>('/api/login/methods').then(
      (m) => {
        if (!live) return
        setState(
          validMethods(m)
            ? { kind: 'ready', methods: { ...m, resetByEmail: m.reset_by_email === true } }
            : { kind: 'error' },
        )
      },
      (err) => {
        if (!live) return
        setState(
          err instanceof ApiError && err.status === 404
            ? { kind: 'ready', methods: { password: true, sso: [], resetByEmail: false } }
            : { kind: 'error' },
        )
      },
    )
    return () => {
      live = false
    }
  }, [attempt])
  const retry = () => {
    setState({ kind: 'loading' })
    setAttempt((n) => n + 1)
  }
  return { state, retry }
}

// What a screen shows until the login methods are known, or when they couldn't be had.
function MethodsPending({ state, retry }: { state: MethodsState; retry: () => void }) {
  const t = useT()
  if (state.kind !== 'error') return <AuthLoading />
  return (
    <AuthFrame title={t('auth.methods.title')}>
      <p role="alert" className="tt-error">
        {t('auth.methods.error')}
      </p>
      <button type="button" className="tt-button tt-primary" onClick={retry}>
        {t('auth.methods.retry')}
      </button>
    </AuthFrame>
  )
}

// One button per single sign-on provider. `next` is where the provider's sign-in brings the
// person back to, a path on this server.
function SsoButtons({ providers, next }: { providers: LoginMethods['sso']; next: string }) {
  const t = useT()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  if (providers.length === 0) return null
  const go = async (id: string) => {
    setBusy(true)
    setError(null)
    try {
      const { url } = await api<{ url: string }>(`/api/sso/${encodeURIComponent(id)}/start`, {
        method: 'POST',
        body: { next },
      })
      window.location.assign(url)
    } catch (err) {
      setError(errorText(err))
      setBusy(false)
    }
  }
  return (
    <div className="tt-sso">
      {providers.map((p) => (
        <button key={p.id} type="button" className="tt-button" disabled={busy} onClick={() => void go(p.id)}>
          {t('auth.sso.continue', { name: p.name })}
        </button>
      ))}
      {error && (
        <p role="alert" className="tt-error">
          {error}
        </p>
      )}
    </div>
  )
}

// Why a sign-in with a provider failed: the server's redirect to /?sso_error=<code>. It is read
// once and taken out of the address, so a reload doesn't show it again.
function useSsoError(): string | null {
  const [error] = useState(() => {
    const code = new URLSearchParams(window.location.search).get('sso_error')
    return code === null ? null : codeText(code, undefined, t('errors.sso_failed'))
  })
  useEffect(() => {
    const url = new URL(window.location.href)
    if (url.searchParams.has('sso_error')) {
      url.searchParams.delete('sso_error')
      window.history.replaceState(window.history.state, '', url.pathname + url.search + url.hash)
    }
  }, [])
  return error
}

// The page's path and fragment: where a provider's sign-in comes back to.
const here = () => window.location.pathname + window.location.hash

export function SetupScreen({ onDone }: { onDone: (me: Me) => void }) {
  const t = useT()
  const [form, setForm] = useState(() => ({
    setup_token: '',
    name: '',
    email: '',
    password: '',
    workspace_name: t('auth.setup.defaultWorkspace'),
  }))
  const set = (key: keyof typeof form) => (e: { target: { value: string } }) =>
    setForm((f) => ({ ...f, [key]: e.target.value }))
  const { busy, error, submit } = useSubmit(
    () => api<Me>('/api/setup', { method: 'POST', body: { ...form, setup_token: form.setup_token.trim() } }),
    onDone,
  )
  return (
    <AuthFrame
      title={t('auth.setup.title')}
      intro={<p className="tt-muted">{t.rich('auth.setup.intro', { code: (text) => <code>{text}</code> })}</p>}
    >
      <form className="tt-form" onSubmit={submit}>
        <label>
          {t('auth.setup.token')}
          <input
            value={form.setup_token}
            onChange={set('setup_token')}
            required
            autoComplete="off"
            spellCheck={false}
          />
        </label>
        <label>
          {t('auth.yourName')}
          <input value={form.name} onChange={set('name')} required maxLength={100} autoComplete="name" />
        </label>
        <label>
          {t('auth.email')}
          <input type="email" value={form.email} onChange={set('email')} required autoComplete="email" />
        </label>
        <label>
          {t('auth.password')}
          <input
            type="password"
            value={form.password}
            onChange={set('password')}
            required
            minLength={12}
            autoComplete="new-password"
            aria-describedby="tt-password-hint"
          />
        </label>
        <span id="tt-password-hint" className="tt-hint">
          {t('auth.passwordHint', { min: 12 })}
        </span>
        <label>
          {t('auth.setup.workspaceName')}
          <input value={form.workspace_name} onChange={set('workspace_name')} required maxLength={100} />
        </label>
        {error && (
          <p role="alert" className="tt-error">
            {error}
          </p>
        )}
        <button type="submit" className="tt-button tt-primary" disabled={busy}>
          {busy ? t('auth.setup.busy') : t('auth.setup.submit')}
        </button>
      </form>
    </AuthFrame>
  )
}

export function LoginScreen({ onDone }: { onDone: (me: Me) => void }) {
  const t = useT()
  const loaded = useLoginMethods()
  const ssoError = useSsoError()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  // The challenge of a login whose password was right and that now asks for a code.
  const [challenge, setChallenge] = useState<string | null>(null)
  const { busy, error, submit } = useSubmit(async () => {
    const reply = await api<Me | Challenged>('/api/login', {
      method: 'POST',
      body: { email, password },
    })
    if (needsCode(reply)) {
      setPassword('')
      setChallenge(reply.challenge)
      return null
    }
    return reply
  }, onDone)
  const [forgot, setForgot] = useState(false)
  if (loaded.state.kind !== 'ready') return <MethodsPending state={loaded.state} retry={loaded.retry} />
  if (challenge) return <TwoFactorStep challenge={challenge} onDone={onDone} onBack={() => setChallenge(null)} />
  const methods = loaded.state.methods
  if (forgot && methods.password && methods.resetByEmail) {
    return <ForgotPasswordScreen initialEmail={email} onBack={() => setForgot(false)} />
  }
  return (
    <AuthFrame title={t('auth.login.title')}>
      {ssoError && (
        <p role="alert" className="tt-error">
          {ssoError}
        </p>
      )}
      <SsoButtons providers={methods.sso} next={here()} />
      {methods.password && methods.sso.length > 0 && <p className="tt-muted tt-small tt-sso-or">{t('auth.sso.or')}</p>}
      {methods.password && (
        <form className="tt-form" onSubmit={submit}>
          <label>
            {t('auth.email')}
            <input
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              required
              autoComplete="username"
            />
          </label>
          <label>
            {t('auth.password')}
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
              autoComplete="current-password"
            />
          </label>
          {error && (
            <p role="alert" className="tt-error">
              {error}
            </p>
          )}
          <button type="submit" className="tt-button tt-primary" disabled={busy}>
            {busy ? t('auth.login.busy') : t('auth.login.submit')}
          </button>
          {methods.resetByEmail ? (
            <button type="button" className="tt-button tt-small" onClick={() => setForgot(true)}>
              {t('auth.login.forgotByEmail')}
            </button>
          ) : (
            <p className="tt-muted tt-small">{t('auth.login.forgot')}</p>
          )}
        </form>
      )}
    </AuthFrame>
  )
}

// The second step of a login for someone with two-factor login on: a code from the authenticator
// app, or a recovery code. A challenge that ran out or was used up goes back to the password.
// It is the step of a password login, of an invite taken with an account and of a reset link
// (`reset`: the password is set, and going back means the login). `finish` is what is left to do
// with the session the code made (accepting an invite); if it fails the code isn't asked for again,
// since the challenge is used up and the person is signed in.
function TwoFactorStep({
  challenge,
  onDone,
  onBack,
  reset = false,
  finish,
}: {
  challenge: string
  onDone: (me: Me) => void
  onBack: () => void
  reset?: boolean
  finish?: (me: Me) => Promise<Me>
}) {
  const t = useT()
  const [code, setCode] = useState('')
  const [expired, setExpired] = useState(false)
  const [verified, setVerified] = useState<Me | null>(null)
  const { busy, error, submit } = useSubmit(async () => {
    let me = verified
    if (!me) {
      try {
        me = await api<Me>('/api/login/two-factor', { method: 'POST', body: { challenge, code } })
      } catch (err) {
        if (err instanceof ApiError && err.code === 'two_factor_challenge_invalid') setExpired(true)
        throw err
      }
      setVerified(me)
    }
    return finish ? finish(me) : me
  }, onDone)
  return (
    <AuthFrame
      title={t('auth.twoFactor.title')}
      intro={<p className="tt-muted">{t(reset ? 'auth.twoFactor.introReset' : 'auth.twoFactor.intro')}</p>}
    >
      <form className="tt-form" onSubmit={submit}>
        <label>
          {t('auth.twoFactor.code')}
          <input
            value={code}
            onChange={(e) => setCode(e.target.value)}
            required
            disabled={expired || verified !== null}
            autoComplete="one-time-code"
            spellCheck={false}
          />
        </label>
        {error && (
          <p role="alert" className="tt-error">
            {error}
          </p>
        )}
        <div className="tt-row-actions">
          <button type="submit" className="tt-button tt-primary" disabled={busy || expired}>
            {busy ? t('auth.twoFactor.busy') : t('auth.twoFactor.submit')}
          </button>
          <button type="button" className="tt-button" onClick={onBack}>
            {reset ? t('auth.twoFactor.toLogin') : expired ? t('auth.twoFactor.again') : t('auth.twoFactor.back')}
          </button>
        </div>
        <p className="tt-muted tt-small">{t('auth.twoFactor.lost')}</p>
      </form>
    </AuthFrame>
  )
}

// Asking for a reset link by email, from the login page. The server answers the same whether or
// not an account has the address, and so does this: it never says which.
function ForgotPasswordScreen({ initialEmail, onBack }: { initialEmail: string; onBack: () => void }) {
  const t = useT()
  const [email, setEmail] = useState(initialEmail)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [sentTo, setSentTo] = useState<string | null>(null)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await api('/api/password-reset/request', { method: 'POST', body: { email } })
      setSentTo(email.trim())
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }
  if (sentTo !== null) {
    return (
      <AuthFrame title={t('auth.forgot.sentTitle')}>
        <p>{t.rich('auth.forgot.sent', { email: <strong>{sentTo}</strong> })}</p>
        <p className="tt-muted tt-small">{t('auth.forgot.sentHelp')}</p>
        <button type="button" className="tt-button" onClick={onBack}>
          {t('auth.forgot.back')}
        </button>
      </AuthFrame>
    )
  }
  return (
    <AuthFrame title={t('auth.forgot.title')} intro={<p className="tt-muted">{t('auth.forgot.intro')}</p>}>
      <form className="tt-form" onSubmit={submit}>
        <label>
          {t('auth.email')}
          <input
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            required
            autoComplete="username"
          />
        </label>
        {error && (
          <p role="alert" className="tt-error">
            {error}
          </p>
        )}
        <button type="submit" className="tt-button tt-primary" disabled={busy}>
          {busy ? t('auth.forgot.busy') : t('auth.forgot.submit')}
        </button>
        <button type="button" className="tt-button tt-small" onClick={onBack}>
          {t('auth.forgot.back')}
        </button>
      </form>
    </AuthFrame>
  )
}

// A password reset link, /reset-password#<token>, which an admin made: the token is in the
// fragment, so it never reaches a server log or a Referer. Setting the password signs the
// account out everywhere, and in here.
export function ResetPasswordScreen({ token, onDone }: { token: string; onDone: (me: Me) => void }) {
  const t = useT()
  const loaded = useLoginMethods()
  const [password, setPassword] = useState('')
  const [again, setAgain] = useState('')
  const mismatch = again !== '' && password !== again
  // With two-factor login on the link sets the password and asks for a code: no session before.
  const [challenge, setChallenge] = useState<string | null>(null)
  const { busy, error, submit } = useSubmit(async () => {
    const reply = await api<Me | Challenged>('/api/password-reset', { method: 'POST', body: { token, password } })
    if (needsCode(reply)) {
      setPassword('')
      setAgain('')
      setChallenge(reply.challenge)
      return null
    }
    return reply
  }, onDone)
  // The link is used up, so there is no form to go back to: back is the login, with the new password.
  if (challenge) {
    return <TwoFactorStep challenge={challenge} onDone={onDone} onBack={() => window.location.assign('/')} reset />
  }
  if (!token) {
    return (
      <AuthFrame title={t('auth.reset.title')}>
        <p role="alert" className="tt-error">
          {t('auth.reset.incomplete')}
        </p>
      </AuthFrame>
    )
  }
  if (loaded.state.kind !== 'ready') return <MethodsPending state={loaded.state} retry={loaded.retry} />
  if (!loaded.state.methods.password) {
    // The server refuses the reset when passwords are off: don't offer a form that can't work.
    return (
      <AuthFrame title={t('auth.reset.title')}>
        <p role="alert" className="tt-error">
          {t('auth.reset.off')}
        </p>
      </AuthFrame>
    )
  }
  return (
    <AuthFrame title={t('auth.reset.title')} intro={<p className="tt-muted">{t('auth.reset.intro')}</p>}>
      <form className="tt-form" onSubmit={(e) => (mismatch ? e.preventDefault() : submit(e))}>
        <label>
          {t('auth.reset.password')}
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            minLength={12}
            autoComplete="new-password"
            aria-describedby="tt-password-hint"
          />
        </label>
        <span id="tt-password-hint" className="tt-hint">
          {t('auth.reset.hint')}
        </span>
        <label>
          {t('auth.reset.again')}
          <input
            type="password"
            value={again}
            onChange={(e) => setAgain(e.target.value)}
            required
            autoComplete="new-password"
            aria-invalid={mismatch || undefined}
          />
        </label>
        {(mismatch || error) && (
          <p role="alert" className="tt-error">
            {mismatch ? t('auth.reset.mismatch') : error}
          </p>
        )}
        <button type="submit" className="tt-button tt-primary" disabled={busy || mismatch}>
          {busy ? t('auth.reset.setting') : t('auth.reset.submit')}
        </button>
      </form>
    </AuthFrame>
  )
}

// An invite link, /invite#<token>: the token is in the fragment, so it never reaches a
// server log or a Referer. A new person creates their account with it. Someone signed in
// joins with it, as the invited address only. An existing account that isn't signed in
// logs in first, then joins.
export function InviteScreen({
  token,
  me,
  onDone,
  onLogout,
}: {
  token: string
  me: Me | null
  onDone: (me: Me, before: Me | null) => void
  onLogout: () => void
}) {
  const t = useT()
  const loaded = useLoginMethods()
  const [mode, setMode] = useState<'new' | 'login'>('new')
  // Who the person was before joining, to tell the workspace they joined.
  const before = useRef(me)
  const [form, setForm] = useState({ name: '', password: '', email: '' })
  const set = (key: keyof typeof form) => (e: { target: { value: string } }) =>
    setForm((f) => ({ ...f, [key]: e.target.value }))
  const accept = (body: object) => api<Me>('/api/invites/accept', { method: 'POST', body: { token, ...body } })
  // An existing account with two-factor login on gets a challenge from the login, not a session:
  // the code comes first, and the invite is accepted after it.
  const [challenge, setChallenge] = useState<string | null>(null)
  const { busy, error, submit } = useSubmit(
    async () => {
      if (me) return accept({})
      if (mode === 'login') {
        const reply = await api<Me | Challenged>('/api/login', {
          method: 'POST',
          body: { email: form.email, password: form.password },
        })
        if (needsCode(reply)) {
          setForm((f) => ({ ...f, password: '' }))
          setChallenge(reply.challenge)
          return null
        }
        before.current = reply
        return accept({})
      }
      try {
        return await accept({ name: form.name, password: form.password })
      } catch (e) {
        // The address has an account: log in to it, then accept.
        if (e instanceof ApiError && e.status === 409) {
          setMode('login')
          setForm((f) => ({ ...f, password: '' }))
        }
        throw e
      }
    },
    (joined) => onDone(joined, before.current),
  )

  if (challenge) {
    return (
      <TwoFactorStep
        challenge={challenge}
        onDone={(joined) => onDone(joined, before.current)}
        onBack={() => setChallenge(null)}
        finish={(loggedIn) => {
          before.current = loggedIn
          return accept({})
        }}
      />
    )
  }
  if (!token) {
    return (
      <AuthFrame title={t('auth.invite.title')}>
        <p role="alert" className="tt-error">
          {t('auth.invite.incomplete')}
        </p>
      </AuthFrame>
    )
  }
  if (me) {
    return (
      <AuthFrame
        title={t('auth.invite.title')}
        intro={
          <p className="tt-muted">{t.rich('auth.invite.loggedInAs', { email: <strong>{me.user.email}</strong> })}</p>
        }
      >
        <form className="tt-form" onSubmit={submit}>
          {error && (
            <p role="alert" className="tt-error">
              {error}
            </p>
          )}
          <button type="submit" className="tt-button tt-primary" disabled={busy}>
            {busy ? t('auth.invite.joining') : t('auth.invite.join')}
          </button>
          <button type="button" className="tt-button" onClick={onLogout}>
            {t('auth.invite.logOut')}
          </button>
        </form>
      </AuthFrame>
    )
  }
  if (loaded.state.kind !== 'ready') return <MethodsPending state={loaded.state} retry={loaded.retry} />
  const methods = loaded.state.methods
  // Signing in with a provider comes back to this invite, signed in.
  const sso = <SsoButtons providers={methods.sso} next={`/invite#${token}`} />
  const or = methods.password && methods.sso.length > 0 && (
    <p className="tt-muted tt-small tt-sso-or">{t('auth.sso.or')}</p>
  )
  if (!methods.password) {
    return (
      <AuthFrame title={t('auth.invite.title')} intro={<p className="tt-muted">{t('auth.invite.ssoIntro')}</p>}>
        {sso}
      </AuthFrame>
    )
  }
  if (mode === 'login') {
    return (
      <AuthFrame title={t('auth.invite.loginTitle')} intro={<p className="tt-muted">{t('auth.invite.loginIntro')}</p>}>
        {sso}
        {or}
        <form className="tt-form" onSubmit={submit}>
          <label>
            {t('auth.email')}
            <input type="email" value={form.email} onChange={set('email')} required autoComplete="username" />
          </label>
          <label>
            {t('auth.password')}
            <input
              type="password"
              value={form.password}
              onChange={set('password')}
              required
              autoComplete="current-password"
            />
          </label>
          {error && (
            <p role="alert" className="tt-error">
              {error}
            </p>
          )}
          <button type="submit" className="tt-button tt-primary" disabled={busy}>
            {busy ? t('auth.invite.joining') : t('auth.invite.loginSubmit')}
          </button>
        </form>
      </AuthFrame>
    )
  }
  return (
    <AuthFrame title={t('auth.invite.title')} intro={<p className="tt-muted">{t('auth.invite.newIntro')}</p>}>
      {sso}
      {or}
      <form className="tt-form" onSubmit={submit}>
        <label>
          {t('auth.yourName')}
          <input value={form.name} onChange={set('name')} required maxLength={100} autoComplete="name" />
        </label>
        <label>
          {t('auth.password')}
          <input
            type="password"
            value={form.password}
            onChange={set('password')}
            required
            minLength={12}
            autoComplete="new-password"
            aria-describedby="tt-password-hint"
          />
        </label>
        <span id="tt-password-hint" className="tt-hint">
          {t('auth.passwordHint', { min: 12 })}
        </span>
        {error && (
          <p role="alert" className="tt-error">
            {error}
          </p>
        )}
        <button type="submit" className="tt-button tt-primary" disabled={busy}>
          {busy ? t('auth.invite.creating') : t('auth.invite.create')}
        </button>
      </form>
    </AuthFrame>
  )
}
