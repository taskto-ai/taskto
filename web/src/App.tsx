// The session decides the screen: setup while the server has no account, login without a
// session, and the shell with one. The URL is kept through login, so a link still opens
// where it pointed. An invite link (/invite#<token>) opens the invite, with a session or
// without, and then the workspace it joined. A password reset link (/reset-password#<token>)
// sets the password, and signs in.

import { useCallback, useEffect, useState } from 'react'
import { ApiError, api, errorText, type Me, SESSION_LOST } from './api'
import { InviteScreen, LoginScreen, ResetPasswordScreen, SetupScreen } from './auth/AuthScreens'
import { setPreferences, useT } from './i18n'
import { navigate } from './router'
import { Shell } from './shell/Shell'
import { claimDrafts, forgetDrafts } from './uiState'

type Session =
  | { state: 'loading' }
  | { state: 'setup' }
  | { state: 'login' }
  | { state: 'in'; me: Me }
  | { state: 'invite'; me: Me | null }
  | { state: 'reset' }
  | { state: 'error'; error: string }

const INVITE_PATH = '/invite'
const RESET_PATH = '/reset-password'

export function App() {
  const t = useT()
  const [session, setSession] = useState<Session>({ state: 'loading' })
  // Unsent drafts kept in this browser belong to whoever is signed in: they are claimed before
  // anything is drawn, so another account never finds the last one's.
  const signIn = useCallback((me: Me) => {
    claimDrafts(me.user.id)
    setSession({ state: 'in', me })
  }, [])

  const load = useCallback(async () => {
    try {
      const setup = await api<{ needed: boolean }>('/api/setup')
      if (setup.needed) return setSession({ state: 'setup' })
      if (window.location.pathname === RESET_PATH) return setSession({ state: 'reset' })
      if (window.location.pathname === INVITE_PATH) {
        const me = await api<Me>('/api/me').catch((e) => {
          if (e instanceof ApiError && e.status === 401) return null
          throw e
        })
        return setSession({ state: 'invite', me })
      }
      const me = await api<Me>('/api/me')
      await setPreferences({ user: me.user.locale ?? null })
      signIn(me)
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) setSession({ state: 'login' })
      else setSession({ state: 'error', error: errorText(e) })
    }
  }, [signIn])

  useEffect(() => {
    load()
  }, [load])

  const signedIn = session.state === 'in'
  useEffect(() => {
    if (!signedIn) return
    const lost = () => setSession({ state: 'login' })
    window.addEventListener(SESSION_LOST, lost)
    return () => window.removeEventListener(SESSION_LOST, lost)
  }, [signedIn])

  const logIn = useCallback(
    async (me: Me) => {
      await setPreferences({ user: me.user.locale ?? null })
      signIn(me)
    },
    [signIn],
  )
  const logOut = useCallback(async () => {
    await api('/api/logout', { method: 'POST' }).catch(() => {})
    forgetDrafts()
    setSession({ state: 'login' })
  }, [])
  // Joined: on to the workspace the invite was for, the one that is new, and the token
  // leaves the address bar and the history entry.
  const joined = useCallback(
    (me: Me, before: Me | null) => {
      const known = new Set(before?.workspaces.map((w) => w.id))
      const workspace = me.workspaces.find((w) => !known.has(w.id)) ?? me.workspaces[0]
      navigate(workspace ? `/w/${workspace.id}/conversations` : '/', { replace: true })
      void setPreferences({ user: me.user.locale ?? null })
      signIn(me)
    },
    [signIn],
  )
  // The new password is set: into the app, and the token leaves the address bar.
  const resetDone = useCallback(
    (me: Me) => {
      navigate('/', { replace: true })
      void setPreferences({ user: me.user.locale ?? null })
      signIn(me)
    },
    [signIn],
  )
  const leaveInvite = useCallback(async () => {
    await api('/api/logout', { method: 'POST' }).catch(() => {})
    forgetDrafts()
    setSession({ state: 'invite', me: null })
  }, [])

  switch (session.state) {
    case 'loading':
      return <p className="tt-center tt-muted">{t('app.loading')}</p>
    case 'setup':
      return <SetupScreen onDone={logIn} />
    case 'login':
      return <LoginScreen onDone={logIn} />
    case 'invite':
      return (
        <InviteScreen token={window.location.hash.slice(1)} me={session.me} onDone={joined} onLogout={leaveInvite} />
      )
    case 'reset':
      return <ResetPasswordScreen token={window.location.hash.slice(1)} onDone={resetDone} />
    case 'error':
      return (
        <div className="tt-center">
          <p role="alert" className="tt-error">
            {t('app.unreachable', { error: session.error })}
          </p>
          <button type="button" className="tt-button" onClick={load}>
            {t('app.tryAgain')}
          </button>
        </div>
      )
    case 'in':
      return <Shell me={session.me} onLogout={logOut} />
  }
}
