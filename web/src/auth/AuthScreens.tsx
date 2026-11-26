// The screens before the app: first-run setup, logging in, invites and password reset links.
// Each answers with the GET /api/me body, which the caller takes as the session.

import { type FormEvent, type ReactNode, useEffect, useRef, useState } from 'react'
import { ApiError, api, codeText, errorText, type Me } from '../api'
import { t, useFormat, useT } from '../i18n'
import { Button } from '../ui/Button'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { AuthDivider, AuthForm, AuthLayout } from './AuthLayout'
import { DELETION_EVENT, takeDeletion } from './deleted'
import { PasswordInput } from './PasswordInput'

// The shortest password the server takes (internal/auth/password.go).
const MIN_PASSWORD = 12

// Where the provider's hosted payment page sends the payer back to: the server's own pages, which
// never come from an app's definition. The payer is a customer of whoever sent the link, often with
// no account here, so these pages call nothing and show nothing of the payment: the provider told
// them already, and what happened to the record is the business's to say.
export function PaymentReturnScreen({ outcome }: { outcome: 'done' | 'cancelled' }) {
  const t = useT()
  return (
    <AuthLayout
      title={outcome === 'done' ? t('payment.done.title') : t('payment.cancelled.title')}
      help={<p>{outcome === 'done' ? t('payment.done.body') : t('payment.cancelled.body')}</p>}
    />
  )
}

// What is shown before the app knows what to show: the logo and the shape of a form, still. A
// screen reader hears that it is loading.
export function StartingScreen() {
  const t = useT()
  return (
    <AuthLayout busy>
      <div className="ui-auth-skeleton">
        <VisuallyHidden>{t('app.loading')}</VisuallyHidden>
        <Skeleton width="60%" height="var(--space-6)" />
        <Skeleton width="85%" />
        <Skeleton shape="block" height="var(--control-md)" />
        <Skeleton shape="block" height="var(--control-md)" />
      </div>
    </AuthLayout>
  )
}

// What a password step (a login, a reset link) answers for an account with two-factor login on:
// no session, a challenge for POST /api/login/two-factor.
type Challenged = { two_factor_required: true; challenge: string }
const needsCode = (reply: Me | Challenged): reply is Challenged => 'two_factor_required' in reply

// Why a step failed: the words to show, and the server's code, which says which field it is about.
type Failure = { text: string; code?: string }

// The field each of the server's codes is about. A failure whose field is on the form shows under
// that field; any other shows above the button.
const FIELD_OF: Record<string, string> = {
  setup_token: 'setup_token',
  email_invalid: 'email',
  name_length: 'name',
  workspace_name_length: 'workspace_name',
  password_too_short: 'password',
  password_too_long: 'password',
  wrong_two_factor_code: 'code',
}

// Where a failure shows on a form with these fields: under one of them, or for the whole form.
function placed(failure: Failure | null, fields: readonly string[]) {
  const field = failure?.code ? FIELD_OF[failure.code] : undefined
  const onField = field !== undefined && fields.includes(field)
  return {
    field: (name: string) => (failure && onField && field === name ? failure.text : undefined),
    // It takes the focus after a failed submit (AuthForm), so it can be focused, but isn't a tab stop.
    form:
      failure && !onField ? (
        <div className="ui-auth-error" tabIndex={-1}>
          <Notice tone="danger">{failure.text}</Notice>
        </div>
      ) : null,
  }
}

const failureOf = (err: unknown): Failure => ({
  text: errorText(err),
  code: err instanceof ApiError ? err.code : undefined,
})

// A failure about one field goes once that field is edited: the value it was about isn't there any
// more. The form reports which field changed by its name (AuthForm's onEdit), and the names are the
// ones FIELD_OF gives.
function useEdited(failure: Failure | null, setFailure: (f: Failure | null) => void) {
  return (name: string) => {
    if (failure?.code && FIELD_OF[failure.code] === name) setFailure(null)
  }
}

// send may answer null: the step is done but nobody is logged in yet (a login that asks for a code).
function useSubmit(send: () => Promise<Me | null>, onDone: (me: Me) => void) {
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<Failure | null>(null)
  const edited = useEdited(failure, setFailure)
  // One request at a time, whatever submits the form: the button ignores a press while it works,
  // but a form can be submitted another way (requestSubmit, a browser or an extension). A ref, not
  // `busy`: the state this function closed over is the one from before the first submit.
  const inFlight = useRef(false)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (inFlight.current) return
    inFlight.current = true
    setBusy(true)
    setFailure(null)
    // The mark goes where `busy` goes, never earlier: after a success the screen is on its way out
    // (onDone may still be opening the account's offline data), and a submit meanwhile would make
    // a second session or be refused.
    const settle = () => {
      inFlight.current = false
      setBusy(false)
    }
    try {
      const me = await send()
      if (me) onDone(me)
      else settle()
    } catch (err) {
      setFailure(failureOf(err))
      settle()
    }
  }
  return { busy, failure, submit, edited }
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

// What a screen shows until the login methods are known, or when they couldn't be had. Until the
// server says how people sign in, nothing is shown that it might take back: no heading or form
// yet, so a screen is never drawn twice.
function MethodsPending({ state, retry }: { state: MethodsState; retry: () => void }) {
  const t = useT()
  if (state.kind !== 'error') return <StartingScreen />
  return (
    <AuthLayout title={t('auth.methods.title')}>
      <div className="ui-auth-stack">
        <Notice tone="danger">{t('auth.methods.error')}</Notice>
        <Button variant="primary" onClick={retry}>
          {t('auth.methods.retry')}
        </Button>
      </div>
    </AuthLayout>
  )
}

// One button per single sign-on provider. `next` is where the provider's sign-in brings the
// person back to, a path on this server.
function SsoButtons({ providers, next }: { providers: LoginMethods['sso']; next: string }) {
  const t = useT()
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  // One start at a time, whatever presses: the state this function closed over can be stale.
  const inFlight = useRef(false)
  if (providers.length === 0) return null
  const go = async (id: string) => {
    if (inFlight.current) return
    inFlight.current = true
    setBusy(id)
    setError(null)
    try {
      const { url } = await api<{ url: string }>(`/api/sso/${encodeURIComponent(id)}/start`, {
        method: 'POST',
        body: { next },
      })
      // The page is leaving for the provider: it stays started.
      window.location.assign(url)
    } catch (err) {
      setError(errorText(err))
      setBusy(null)
      inFlight.current = false
    }
  }
  return (
    <div className="ui-auth-stack">
      {providers.map((p) => (
        <Button
          key={p.id}
          loading={busy === p.id}
          disabled={busy !== null && busy !== p.id}
          onClick={() => void go(p.id)}
        >
          {t('auth.sso.continue', { name: p.name })}
        </Button>
      ))}
      {error && <Notice tone="danger">{error}</Notice>}
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

// An email address field, the same on every form: the keyboard for addresses, no capital letter
// or correction, and filled in by a password manager as the account's name.
function EmailField({
  value,
  onChange,
  error,
  autoComplete = 'username',
}: {
  value: string
  onChange: (value: string) => void
  error?: ReactNode
  autoComplete?: 'username' | 'email'
}) {
  const t = useT()
  return (
    <Field label={t('auth.email')} error={error}>
      <Input
        type="email"
        name="email"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        required
        autoComplete={autoComplete}
        autoCapitalize="none"
        autoCorrect="off"
        spellCheck={false}
      />
    </Field>
  )
}

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
  const { busy, failure, submit, edited } = useSubmit(
    () => api<Me>('/api/setup', { method: 'POST', body: { ...form, setup_token: form.setup_token.trim() } }),
    onDone,
  )
  const errors = placed(failure, ['setup_token', 'name', 'email', 'password', 'workspace_name'])
  return (
    <AuthLayout title={t('auth.setup.title')} help={<p>{t('auth.setup.lede')}</p>}>
      <AuthForm key="setup" failure={failure} onEdit={edited} onSubmit={submit}>
        <Field
          label={t('auth.setup.token')}
          hint={t.rich('auth.setup.tokenHint', { code: (text) => <code className="ui-auth-mono">{text}</code> })}
          error={errors.field('setup_token')}
        >
          <Input
            name="setup_token"
            value={form.setup_token}
            onChange={set('setup_token')}
            required
            autoComplete="off"
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
          />
        </Field>
        <Field label={t('auth.yourName')} error={errors.field('name')}>
          <Input name="name" value={form.name} onChange={set('name')} required maxLength={100} autoComplete="name" />
        </Field>
        <EmailField
          value={form.email}
          onChange={(email) => setForm((f) => ({ ...f, email }))}
          error={errors.field('email')}
          autoComplete="email"
        />
        <Field
          label={t('auth.password')}
          hint={t('auth.passwordHint', { min: MIN_PASSWORD })}
          error={errors.field('password')}
        >
          <PasswordInput
            name="password"
            value={form.password}
            onChange={set('password')}
            required
            minLength={MIN_PASSWORD}
            autoComplete="new-password"
          />
        </Field>
        <Field label={t('auth.setup.workspaceName')} error={errors.field('workspace_name')}>
          <Input
            name="workspace_name"
            value={form.workspace_name}
            onChange={set('workspace_name')}
            required
            maxLength={100}
            autoComplete="organization"
          />
        </Field>
        {errors.form}
        <Button type="submit" variant="primary" loading={busy}>
          {t('auth.setup.submit')}
        </Button>
      </AuthForm>
    </AuthLayout>
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
  const { busy, failure, submit, edited } = useSubmit(async () => {
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
  // Told once, to a person who has just deleted their account.
  const [deleted, setDeleted] = useState(takeDeletion)
  // ...or told now, if the deletion is answered after this screen is up.
  useEffect(() => {
    const told = () => {
      const at = takeDeletion()
      if (at) setDeleted(at)
    }
    window.addEventListener(DELETION_EVENT, told)
    return () => window.removeEventListener(DELETION_EVENT, told)
  }, [])
  const f = useFormat()
  if (loaded.state.kind !== 'ready') return <MethodsPending state={loaded.state} retry={loaded.retry} />
  if (challenge) return <TwoFactorStep challenge={challenge} onDone={onDone} onBack={() => setChallenge(null)} />
  const methods = loaded.state.methods
  if (forgot && methods.password && methods.resetByEmail) {
    return <ForgotPasswordScreen initialEmail={email} onBack={() => setForgot(false)} />
  }
  const errors = placed(failure, ['email', 'password'])
  return (
    <AuthLayout
      title={t('auth.login.title')}
      links={
        methods.password &&
        (methods.resetByEmail ? (
          <Button variant="ghost" size="sm" onClick={() => setForgot(true)}>
            {t('auth.login.forgotByEmail')}
          </Button>
        ) : (
          <p>{t('auth.login.forgot')}</p>
        ))
      }
    >
      {deleted && <Notice tone="info">{t('auth.accountDeleted', { date: f.date(deleted) })}</Notice>}
      {ssoError && <Notice tone="danger">{ssoError}</Notice>}
      <SsoButtons providers={methods.sso} next={here()} />
      {methods.password && methods.sso.length > 0 && <AuthDivider>{t('auth.sso.or')}</AuthDivider>}
      {methods.password && (
        <AuthForm key="login" failure={failure} onEdit={edited} onSubmit={submit}>
          <EmailField value={email} onChange={setEmail} error={errors.field('email')} />
          <Field label={t('auth.password')} error={errors.field('password')}>
            <PasswordInput
              name="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
              autoComplete="current-password"
            />
          </Field>
          {errors.form}
          <Button type="submit" variant="primary" loading={busy}>
            {t('auth.login.submit')}
          </Button>
        </AuthForm>
      )}
    </AuthLayout>
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
  const { busy, failure, submit, edited } = useSubmit(async () => {
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
  const errors = placed(failure, ['code'])
  return (
    <AuthLayout
      title={t('auth.twoFactor.title')}
      help={<p>{t(reset ? 'auth.twoFactor.introReset' : 'auth.twoFactor.intro')}</p>}
      links={
        <Button variant="ghost" size="sm" icon="back" onClick={onBack}>
          {reset ? t('auth.twoFactor.toLogin') : expired ? t('auth.twoFactor.again') : t('auth.twoFactor.back')}
        </Button>
      }
    >
      <AuthForm key="two-factor" failure={failure} onEdit={edited} onSubmit={submit}>
        <Field label={t('auth.twoFactor.code')} hint={t('auth.twoFactor.lost')} error={errors.field('code')}>
          <Input
            name="code"
            value={code}
            onChange={(e) => setCode(e.target.value)}
            required
            disabled={expired || verified !== null}
            // The authenticator's six digits, which a phone offers from a message; a recovery code
            // has letters, so the keyboard stays the full one.
            autoComplete="one-time-code"
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
          />
        </Field>
        {errors.form}
        <Button type="submit" variant="primary" loading={busy} disabled={expired}>
          {t('auth.twoFactor.submit')}
        </Button>
      </AuthForm>
    </AuthLayout>
  )
}

// Asking for a reset link by email, from the login page. The server answers the same whether or
// not an account has the address, and so does this: it never says which.
function ForgotPasswordScreen({ initialEmail, onBack }: { initialEmail: string; onBack: () => void }) {
  const t = useT()
  const [email, setEmail] = useState(initialEmail)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<Failure | null>(null)
  const edited = useEdited(failure, setFailure)
  const [sentTo, setSentTo] = useState<string | null>(null)
  // One request at a time, as useSubmit.
  const inFlight = useRef(false)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (inFlight.current) return
    inFlight.current = true
    setBusy(true)
    setFailure(null)
    try {
      await api('/api/password-reset/request', { method: 'POST', body: { email } })
      setSentTo(email.trim())
    } catch (err) {
      setFailure(failureOf(err))
    } finally {
      inFlight.current = false
      setBusy(false)
    }
  }
  const back = (
    <Button variant="ghost" size="sm" icon="back" onClick={onBack}>
      {t('auth.forgot.back')}
    </Button>
  )
  if (sentTo !== null) {
    return (
      <AuthLayout
        title={t('auth.forgot.sentTitle')}
        help={<p>{t.rich('auth.forgot.sent', { email: <strong>{sentTo}</strong> })}</p>}
        links={back}
      >
        <p className="ui-auth-note">{t('auth.forgot.sentHelp')}</p>
      </AuthLayout>
    )
  }
  const errors = placed(failure, ['email'])
  return (
    <AuthLayout title={t('auth.forgot.title')} help={<p>{t('auth.forgot.intro')}</p>} links={back}>
      <AuthForm key="forgot" failure={failure} onEdit={edited} onSubmit={submit}>
        <EmailField value={email} onChange={setEmail} error={errors.field('email')} />
        {errors.form}
        <Button type="submit" variant="primary" loading={busy}>
          {t('auth.forgot.submit')}
        </Button>
      </AuthForm>
    </AuthLayout>
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
  const { busy, failure, submit, edited } = useSubmit(async () => {
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
      <AuthLayout title={t('auth.reset.title')}>
        <Notice tone="danger">{t('auth.reset.incomplete')}</Notice>
      </AuthLayout>
    )
  }
  if (loaded.state.kind !== 'ready') return <MethodsPending state={loaded.state} retry={loaded.retry} />
  if (!loaded.state.methods.password) {
    // The server refuses the reset when passwords are off: don't offer a form that can't work.
    return (
      <AuthLayout title={t('auth.reset.title')}>
        <Notice tone="danger">{t('auth.reset.off')}</Notice>
      </AuthLayout>
    )
  }
  const errors = placed(failure, ['password'])
  return (
    <AuthLayout title={t('auth.reset.title')} help={<p>{t('auth.reset.intro')}</p>}>
      <AuthForm
        key="reset"
        failure={failure}
        onEdit={edited}
        onSubmit={(e) => (mismatch ? e.preventDefault() : submit(e))}
      >
        <Field label={t('auth.reset.password')} hint={t('auth.reset.hint')} error={errors.field('password')}>
          <PasswordInput
            name="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            minLength={MIN_PASSWORD}
            autoComplete="new-password"
          />
        </Field>
        <Field label={t('auth.reset.again')} error={mismatch ? t('auth.reset.mismatch') : undefined}>
          <PasswordInput
            name="password-again"
            value={again}
            onChange={(e) => setAgain(e.target.value)}
            required
            autoComplete="new-password"
          />
        </Field>
        {errors.form}
        <Button type="submit" variant="primary" loading={busy} disabled={mismatch}>
          {t('auth.reset.submit')}
        </Button>
      </AuthForm>
    </AuthLayout>
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
  const { busy, failure, submit, edited } = useSubmit(
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
      <AuthLayout title={t('auth.invite.title')}>
        <Notice tone="danger">{t('auth.invite.incomplete')}</Notice>
      </AuthLayout>
    )
  }
  if (me) {
    const errors = placed(failure, [])
    return (
      <AuthLayout
        title={t('auth.invite.title')}
        help={<p>{t.rich('auth.invite.loggedInAs', { email: <strong>{me.user.email}</strong> })}</p>}
        links={
          <Button variant="ghost" size="sm" onClick={onLogout}>
            {t('auth.invite.logOut')}
          </Button>
        }
      >
        <AuthForm key="invite-join" failure={failure} onEdit={edited} onSubmit={submit}>
          {errors.form}
          <Button type="submit" variant="primary" loading={busy}>
            {t('auth.invite.join')}
          </Button>
        </AuthForm>
      </AuthLayout>
    )
  }
  if (loaded.state.kind !== 'ready') return <MethodsPending state={loaded.state} retry={loaded.retry} />
  const methods = loaded.state.methods
  // Signing in with a provider comes back to this invite, signed in.
  const sso = <SsoButtons providers={methods.sso} next={`/invite#${token}`} />
  const or = methods.password && methods.sso.length > 0 && <AuthDivider>{t('auth.sso.or')}</AuthDivider>
  if (!methods.password) {
    return (
      <AuthLayout title={t('auth.invite.title')} help={<p>{t('auth.invite.ssoIntro')}</p>}>
        {sso}
      </AuthLayout>
    )
  }
  if (mode === 'login') {
    const errors = placed(failure, ['email', 'password'])
    return (
      <AuthLayout title={t('auth.invite.loginTitle')} help={<p>{t('auth.invite.loginIntro')}</p>}>
        {sso}
        {or}
        <AuthForm key="invite-login" failure={failure} onEdit={edited} onSubmit={submit}>
          <EmailField
            value={form.email}
            onChange={(email) => setForm((f) => ({ ...f, email }))}
            error={errors.field('email')}
          />
          <Field label={t('auth.password')} error={errors.field('password')}>
            <PasswordInput
              name="password"
              value={form.password}
              onChange={set('password')}
              required
              autoComplete="current-password"
            />
          </Field>
          {errors.form}
          <Button type="submit" variant="primary" loading={busy}>
            {t('auth.invite.loginSubmit')}
          </Button>
        </AuthForm>
      </AuthLayout>
    )
  }
  const errors = placed(failure, ['name', 'password'])
  return (
    <AuthLayout title={t('auth.invite.title')} help={<p>{t('auth.invite.newIntro')}</p>}>
      {sso}
      {or}
      <AuthForm key="invite-new" failure={failure} onEdit={edited} onSubmit={submit}>
        <Field label={t('auth.yourName')} error={errors.field('name')}>
          <Input name="name" value={form.name} onChange={set('name')} required maxLength={100} autoComplete="name" />
        </Field>
        <Field
          label={t('auth.password')}
          hint={t('auth.passwordHint', { min: MIN_PASSWORD })}
          error={errors.field('password')}
        >
          <PasswordInput
            name="password"
            value={form.password}
            onChange={set('password')}
            required
            minLength={MIN_PASSWORD}
            autoComplete="new-password"
          />
        </Field>
        {errors.form}
        <Button type="submit" variant="primary" loading={busy}>
          {t('auth.invite.create')}
        </Button>
      </AuthForm>
    </AuthLayout>
  )
}
