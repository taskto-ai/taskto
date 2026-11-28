import { type FormEvent, type ReactNode, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { ApiError, api, codeText, errorText, type Me } from '../api'
import { t, useFormat, useT } from '../i18n'
import { useLoad } from '../load'
import { Button } from '../ui/Button'
import { Field } from '../ui/Field'
import { Input } from '../ui/Input'
import { Notice } from '../ui/Notice'
import { Skeleton } from '../ui/Skeleton'
import { VisuallyHidden } from '../ui/VisuallyHidden'
import { AuthDivider, AuthForm, AuthLayout } from './AuthLayout'
import { ackDeletion, DELETION_EVENT, peekDeletion, signInSubmitted } from './deleted'
import { PasswordInput } from './PasswordInput'

const MIN_PASSWORD = 12

export function PaymentReturnScreen({ outcome }: { outcome: 'done' | 'cancelled' }) {
  const t = useT()
  return (
    <AuthLayout
      title={outcome === 'done' ? t('payment.done.title') : t('payment.cancelled.title')}
      help={<p>{outcome === 'done' ? t('payment.done.body') : t('payment.cancelled.body')}</p>}
    />
  )
}

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

type Challenged = { two_factor_required: true; challenge: string }
const needsCode = (reply: Me | Challenged): reply is Challenged => 'two_factor_required' in reply

type Failure = { text: string; code?: string }

const FIELD_OF: Record<string, string> = {
  setup_token: 'setup_token',
  email_invalid: 'email',
  name_length: 'name',
  workspace_name_length: 'workspace_name',
  password_too_short: 'password',
  password_too_long: 'password',
  wrong_two_factor_code: 'code',
}

function placed(failure: Failure | null, fields: readonly string[]) {
  const field = failure?.code ? FIELD_OF[failure.code] : undefined
  const onField = field !== undefined && fields.includes(field)
  return {
    field: (name: string) => (failure && onField && field === name ? failure.text : undefined),
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

function useEdited(failure: Failure | null, setFailure: (f: Failure | null) => void) {
  return (name: string) => {
    if (failure?.code && FIELD_OF[failure.code] === name) setFailure(null)
  }
}

function useSubmit(send: () => Promise<Me | null>, onDone: (me: Me) => void) {
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<Failure | null>(null)
  const edited = useEdited(failure, setFailure)
  const inFlight = useRef(false)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (inFlight.current) return
    inFlight.current = true
    setBusy(true)
    setFailure(null)
    signInSubmitted()
    const settle = () => {
      inFlight.current = false
      setBusy(false)
    }
    try {
      const me = await send()
      if (me) {
        ackDeletion()
        onDone(me)
      } else settle()
    } catch (err) {
      setFailure(failureOf(err))
      settle()
    }
  }
  return { busy, failure, submit, edited }
}

type LoginMethods = { password: boolean; sso: { id: string; name: string }[]; resetByEmail: boolean }
type MethodsAnswer = { password: boolean; sso: LoginMethods['sso']; reset_by_email?: boolean }

type MethodsState = { kind: 'loading' } | { kind: 'ready'; methods: LoginMethods } | { kind: 'error' }

function validMethods(v: unknown): v is MethodsAnswer {
  if (typeof v !== 'object' || v === null) return false
  const m = v as { password?: unknown; sso?: unknown; reset_by_email?: unknown }
  return (
    typeof m.password === 'boolean' &&
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

class InvalidMethods extends Error {}

function readMethods(m: unknown): LoginMethods {
  if (!validMethods(m)) throw new InvalidMethods()
  return { ...m, resetByEmail: m.reset_by_email === true }
}

function passwordsOnlyWhenUnknown(err: unknown): LoginMethods {
  if (err instanceof ApiError && err.status === 404) return { password: true, sso: [], resetByEmail: false }
  throw err
}

const loadMethods = () => api<unknown>('/api/login/methods').then(readMethods, passwordsOnlyWhenUnknown)

function useLoginMethods(): { state: MethodsState; retry: () => void } {
  const loaded = useLoad(loadMethods, [])
  const state: MethodsState = loaded.loading
    ? { kind: 'loading' }
    : loaded.data
      ? { kind: 'ready', methods: loaded.data }
      : { kind: 'error' }
  return { state, retry: loaded.reload }
}

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

function SsoButtons({ providers, next }: { providers: LoginMethods['sso']; next: string }) {
  const t = useT()
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
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

const here = () => window.location.pathname + window.location.hash

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
  const [challenge, setChallenge] = useState<string | null>(null)
  const { busy, failure, submit, edited } = useSubmit(async () => {
    setDeleted(null)
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
  const [deleted, setDeleted] = useState(peekDeletion)
  useEffect(() => {
    const told = () => {
      const at = peekDeletion()
      if (at) setDeleted(at)
    }
    window.addEventListener(DELETION_EVENT, told)
    return () => window.removeEventListener(DELETION_EVENT, told)
  }, [])
  const methods = loaded.state.kind === 'ready' ? loaded.state.methods : null
  const forgotShown = forgot && methods?.password && methods.resetByEmail
  const showing = methods !== null && !challenge && !forgotShown ? deleted : null
  useLayoutEffect(() => {
    if (!showing) return
    const ack = () => {
      if (document.visibilityState === 'hidden') return
      ackDeletion(showing)
      document.removeEventListener('visibilitychange', ack)
    }
    ack()
    document.addEventListener('visibilitychange', ack)
    return () => document.removeEventListener('visibilitychange', ack)
  }, [showing])
  const f = useFormat()
  if (!methods) return <MethodsPending state={loaded.state} retry={loaded.retry} />
  if (challenge) return <TwoFactorStep challenge={challenge} onDone={onDone} onBack={() => setChallenge(null)} />
  if (forgotShown) {
    return <ForgotPasswordScreen initialEmail={email} onBack={() => setForgot(false)} onAsk={() => setDeleted(null)} />
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

function ForgotPasswordScreen({
  initialEmail,
  onBack,
  onAsk,
}: {
  initialEmail: string
  onBack: () => void
  onAsk: () => void
}) {
  const t = useT()
  const [email, setEmail] = useState(initialEmail)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<Failure | null>(null)
  const edited = useEdited(failure, setFailure)
  const [sentTo, setSentTo] = useState<string | null>(null)
  const inFlight = useRef(false)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (inFlight.current) return
    inFlight.current = true
    setBusy(true)
    setFailure(null)
    signInSubmitted()
    onAsk()
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

export function ResetPasswordScreen({ token, onDone }: { token: string; onDone: (me: Me) => void }) {
  const t = useT()
  const loaded = useLoginMethods()
  const [password, setPassword] = useState('')
  const [again, setAgain] = useState('')
  const mismatch = again !== '' && password !== again
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
  const before = useRef(me)
  const [form, setForm] = useState({ name: '', password: '', email: '' })
  const set = (key: keyof typeof form) => (e: { target: { value: string } }) =>
    setForm((f) => ({ ...f, [key]: e.target.value }))
  const accept = (body: object) => api<Me>('/api/invites/accept', { method: 'POST', body: { token, ...body } })
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
