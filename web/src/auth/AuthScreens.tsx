// The screens before the app: first-run setup, logging in, invites and password reset links.
// Each answers with the GET /api/me body, which the caller takes as the session.

import { type FormEvent, type ReactNode, useRef, useState } from 'react'
import { ApiError, api, errorText, type Me } from '../api'
import { BrandMark } from '../BrandMark'
import { useT } from '../i18n'

// The product's name, the same in every language.
const BRAND = 'TaskTo.AI'

function AuthFrame({ title, intro, children }: { title: string; intro?: ReactNode; children: ReactNode }) {
  return (
    <main className="tt-auth">
      <div className="tt-auth-card">
        <p className="tt-brand">
          <BrandMark />
          {BRAND}
        </p>
        <h1>{title}</h1>
        {intro}
        {children}
      </div>
    </main>
  )
}

function useSubmit(send: () => Promise<Me>, onDone: (me: Me) => void) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      onDone(await send())
    } catch (err) {
      setError(errorText(err))
      setBusy(false)
    }
  }
  return { busy, error, submit }
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
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const { busy, error, submit } = useSubmit(
    () => api<Me>('/api/login', { method: 'POST', body: { email, password } }),
    onDone,
  )
  return (
    <AuthFrame title={t('auth.login.title')}>
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
        <p className="tt-muted tt-small">{t('auth.login.forgot')}</p>
      </form>
    </AuthFrame>
  )
}

// A password reset link, /reset-password#<token>, which an admin made: the token is in the
// fragment, so it never reaches a server log or a Referer. Setting the password signs the
// account out everywhere, and in here.
export function ResetPasswordScreen({ token, onDone }: { token: string; onDone: (me: Me) => void }) {
  const t = useT()
  const [password, setPassword] = useState('')
  const [again, setAgain] = useState('')
  const mismatch = again !== '' && password !== again
  const { busy, error, submit } = useSubmit(
    () => api<Me>('/api/password-reset', { method: 'POST', body: { token, password } }),
    onDone,
  )
  if (!token) {
    return (
      <AuthFrame title={t('auth.reset.title')}>
        <p role="alert" className="tt-error">
          {t('auth.reset.incomplete')}
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
  const [mode, setMode] = useState<'new' | 'login'>('new')
  // Who the person was before joining, to tell the workspace they joined.
  const before = useRef(me)
  const [form, setForm] = useState({ name: '', password: '', email: '' })
  const set = (key: keyof typeof form) => (e: { target: { value: string } }) =>
    setForm((f) => ({ ...f, [key]: e.target.value }))
  const accept = (body: object) => api<Me>('/api/invites/accept', { method: 'POST', body: { token, ...body } })
  const { busy, error, submit } = useSubmit(
    async () => {
      if (me) return accept({})
      if (mode === 'login') {
        before.current = await api<Me>('/api/login', {
          method: 'POST',
          body: { email: form.email, password: form.password },
        })
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
  if (mode === 'login') {
    return (
      <AuthFrame title={t('auth.invite.loginTitle')} intro={<p className="tt-muted">{t('auth.invite.loginIntro')}</p>}>
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
