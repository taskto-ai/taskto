// The session decides the screen: setup while the server has no account, login without a
// session, and the shell with one. The URL is kept through login, so a link still opens
// where it pointed. An invite link (/invite#<token>) opens the invite, with a session or
// without, and then the workspace it joined. A password reset link (/reset-password#<token>)
// sets the password, and signs in.

import { lazy, Suspense, useCallback, useEffect, useLayoutEffect, useState } from 'react'
import { ACCOUNT_CHANGED_EVENT, ApiError, api, errorText, forAccount, type Me, SESSION_LOST } from './api'
import { InviteScreen, LoginScreen, ResetPasswordScreen, SetupScreen } from './auth/AuthScreens'
import { setPreferences, useT } from './i18n'
import { leavePush } from './notifications/push'
import { useOpenFromWorker } from './notifications/usePushSync'
import { AccountChanged } from './offline/AccountChanged'
import { ForeignGuard } from './offline/ForeignGuard'
import { onAccountSignal, watchAccount } from './offline/account'
import { useRuntime } from './offline/hooks'
import { LogoutGuard } from './offline/LogoutGuard'
import { activate, deactivate, discardForeign, lastUser, settle, type Unsent, unsentOf } from './offline/runtime'
import { approvalOf } from './offline/store'
import { navigate } from './router'
import { beginSession, endSession, sessionUser } from './session'
import { Shell } from './shell/Shell'
import { reportTimeZone } from './timezone'
import { claimDrafts, forgetDrafts } from './uiState'

// The renderer comes with it: loaded only where it is used, and precached, so it opens offline too.
const OfflineShell = lazy(() => import('./offline/OfflineShell').then((m) => ({ default: m.OfflineShell })))

type Session =
  | { state: 'loading' }
  | { state: 'setup' }
  | { state: 'login' }
  | { state: 'in'; me: Me }
  | { state: 'invite'; me: Me | null }
  | { state: 'reset' }
  // The server can't be reached and someone signed in here before: the apps they downloaded.
  | { state: 'offline'; name: string }
  | { state: 'error'; error: string }

const INVITE_PATH = '/invite'
const RESET_PATH = '/reset-password'

// Signing out is the same from wherever it is asked: who, and where it leaves to.
type Leaving = { user: number; next: Session }
type Asked = Leaving & Unsent

export function App() {
  const t = useT()
  const [session, setSession] = useState<Session>({ state: 'loading' })
  const { runtime, foreign, problem } = useRuntime()
  // Unsent drafts kept in this browser belong to whoever is signed in: they are claimed before
  // anything is drawn, so another account never finds the last one's. What is kept for offline use
  // is the same: this account's, and no other's (another account's unsent records are put to the
  // person first, `foreign`). Sending what the account queued starts (or goes on, after the
  // session ended and this person signed in again).
  const claim = useCallback(async (me: Me) => {
    claimDrafts(me.user.id)
    await activate(me.user.id, me.user.name)
  }, [])
  const signIn = useCallback(
    async (me: Me) => {
      await claim(me)
      // Whichever way they signed in, an account with no time zone gets this browser's, once.
      const now = await reportTimeZone(me)
      // The session begins before the shell is drawn, so what the shell starts is in it.
      beginSession(now.user.id)
      setSession({ state: 'in', me: now })
    },
    [claim],
  )
  // Records made on this device that haven't reached the server: asked about before signing out.
  const [leaving, setLeaving] = useState<Asked | null>(null)

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
        // Signed in on the invite page is signed in: what this device keeps of the account is
        // theirs from here, and is what signing out from the page takes away.
        if (me) {
          await claim(me)
          beginSession(me.user.id)
        }
        return setSession({ state: 'invite', me })
      }
      const me = await api<Me>('/api/me')
      await setPreferences({ user: me.user.locale ?? null })
      await signIn(me)
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) return setSession({ state: 'login' })
      // No answer at all (not a server's error): offline. If someone signed in here, the apps they
      // downloaded can still be used.
      const before = !(e instanceof ApiError) ? lastUser() : null
      if (before) {
        void activate(before.id)
        setSession({ state: 'offline', name: before.name })
      } else {
        setSession({ state: 'error', error: errorText(e) })
      }
    }
  }, [signIn, claim])

  useEffect(() => {
    load()
  }, [load])

  const signedIn = session.state === 'in'
  const account = session.state === 'in' || session.state === 'invite' ? (session.me?.user.id ?? null) : null
  // Another account signed in on this browser, or this one signed out, in another tab: this page's
  // session is over at once (session.ts), and nothing of it is shown until the page is reloaded.
  // Heard here, from the same signals the offline runtime hears, so it holds without a database too
  // (a private window, storage refused); and when the server refuses a request of this account as
  // another's.
  const [changed, setChanged] = useState(false)
  useEffect(() => {
    if (account === null) return
    const over = () => {
      // Already over (signing out here): this tab's own change, not another's.
      if (sessionUser() !== account) return
      endSession()
      setChanged(true)
    }
    watchAccount()
    const off = onAccountSignal((who) => {
      if (who !== account) over()
    })
    const refused = (e: Event) => {
      if ((e as CustomEvent<{ expected?: number }>).detail?.expected === account) over()
    }
    window.addEventListener(ACCOUNT_CHANGED_EVENT, refused)
    return () => {
      off()
      window.removeEventListener(ACCOUNT_CHANGED_EVENT, refused)
    }
  }, [account])
  // The offline runtime found it out first (while activating): the same.
  useLayoutEffect(() => {
    if (problem === 'account') endSession()
  }, [problem])
  // A click on a push in a window that is open already: heard here, not in the shell, so it isn't
  // lost while the login is showing.
  useOpenFromWorker(session.state === 'in' || session.state === 'login' || session.state === 'loading')
  useEffect(() => {
    if (!signedIn) return
    // The session ended elsewhere (or expired): this device stops receiving the person's pushes.
    const lost = () => {
      void leavePush()
      setSession({ state: 'login' })
    }
    window.addEventListener(SESSION_LOST, lost)
    return () => window.removeEventListener(SESSION_LOST, lost)
  }, [signedIn])

  const logIn = useCallback(
    async (me: Me) => {
      await setPreferences({ user: me.user.locale ?? null })
      await signIn(me)
    },
    [signIn],
  )
  // Signing out takes with it what this device kept of the account (drafts, downloads, the queue),
  // and a person with records not yet sent is asked first: every sign-out goes through here.
  // `keep`: the account's database is not deleted (the person chose so, since this version
  // couldn't tell whether it holds unsent records).
  const signOut = useCallback(async ({ user, next, items }: Leaving & Partial<Pick<Unsent, 'items'>>, keep = false) => {
    // The person has asked to go: their session is over from here, before anything below waits, so
    // nothing of theirs still on its way (a toast's button, a save) acts meanwhile. If it turns out
    // they don't sign out after all (records to ask about again), it begins again.
    endSession()
    const approved = approvalOf(items ?? [])
    // What the device holds is decided first and in one step: if a record that was not shown to the
    // person has appeared since (another tab of the account, a stale one), nothing is deleted and
    // nobody is signed out: they are asked again about what is there now.
    if (!keep) {
      const left = await settle(user, approved)
      if (left.length > 0) {
        beginSession(user)
        setLeaving({ user, next, count: left.length, items: left, unreadable: null })
        return
      }
    }
    // This device's push subscription goes first, while the session can still say whose it is.
    await leavePush()
    // The request says whose session it is for: a tab that is stale (another account signed in
    // since) never signs that account out; the server refuses it.
    const refused = await api('/api/logout', { method: 'POST', headers: forAccount(user) }).then(
      () => false,
      (e) => e instanceof ApiError && e.code === 'session_account_changed',
    )
    // Refused: the browser's session is another account's now (another tab). Nothing of this one is
    // shown any more; the page is reloaded to go on as whoever that is.
    if (refused) {
      setChanged(true)
      return
    }
    forgetDrafts()
    const left = await deactivate(keep ? { purge: false, keep: true, user } : { purge: true, user, approved })
    if (left.length > 0) {
      setLeaving({ user, next, count: left.length, items: left, unreadable: null })
      return
    }
    setLeaving(null)
    setSession(next)
  }, [])
  const logOut = useCallback(
    async (who: Leaving) => {
      // A database that can't be checked is not an empty one: the person is told, and chooses.
      const unsent = await unsentOf(who.user)
      if (unsent.count > 0 || unsent.unreadable) setLeaving({ ...who, ...unsent })
      else await signOut(who)
    },
    [signOut],
  )
  // Joined: on to the workspace the invite was for, the one that is new, and the token
  // leaves the address bar and the history entry.
  const joined = useCallback(
    (me: Me, before: Me | null) => {
      const known = new Set(before?.workspaces.map((w) => w.id))
      const workspace = me.workspaces.find((w) => !known.has(w.id)) ?? me.workspaces[0]
      navigate(workspace ? `/w/${workspace.id}/conversations` : '/', { replace: true })
      void setPreferences({ user: me.user.locale ?? null })
      void signIn(me)
    },
    [signIn],
  )
  // The new password is set: into the app, and the token leaves the address bar.
  const resetDone = useCallback(
    (me: Me) => {
      navigate('/', { replace: true })
      void setPreferences({ user: me.user.locale ?? null })
      void signIn(me)
    },
    [signIn],
  )

  // What stands in the way of a signed-in account: records of another account that this browser
  // holds, which the person decides about first (nothing of them is shown).
  const leavingGuard = leaving && (
    <LogoutGuard
      key={leaving.items.map((i) => `${i.id}@${i.version}`).join()}
      runtime={runtime}
      count={leaving.count}
      unreadable={leaving.unreadable}
      onSignOut={() => void signOut(leaving)}
      onKeep={() => void signOut(leaving, true)}
      onCancel={() => setLeaving(null)}
    />
  )
  const foreignGuard = (me: Me) =>
    foreign && (
      <>
        <ForeignGuard
          count={foreign.count}
          onDiscard={() => void discardForeign()}
          onSignOut={() => void logOut({ user: me.user.id, next: { state: 'login' } })}
        />
        {leavingGuard}
      </>
    )

  // Another account is signed in on this browser than the one this page holds: nothing of the
  // account's is shown, and the page is reloaded to go on as whoever is signed in.
  if (problem === 'account' || changed) return <AccountChanged />

  switch (session.state) {
    case 'loading':
      return <p className="tt-center tt-muted">{t('app.loading')}</p>
    case 'setup':
      return <SetupScreen onDone={logIn} />
    case 'login':
      return <LoginScreen onDone={logIn} />
    case 'invite': {
      const me = session.me
      if (me && foreign) return foreignGuard(me)
      return (
        <>
          <InviteScreen
            token={window.location.hash.slice(1)}
            me={me}
            onDone={joined}
            onLogout={() => me && void logOut({ user: me.user.id, next: { state: 'invite', me: null } })}
          />
          {leavingGuard}
        </>
      )
    }
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
    case 'offline':
      return (
        <Suspense fallback={<p className="tt-center tt-muted">{t('app.loading')}</p>}>
          <OfflineShell user={session.name} onReconnect={load} />
        </Suspense>
      )
    case 'in': {
      const me = session.me
      if (foreign) return foreignGuard(me)
      return (
        <>
          <Shell
            me={me}
            onLogout={() => void logOut({ user: me.user.id, next: { state: 'login' } })}
            onMe={(next) =>
              // Only the account still signed in: an answer that comes after logout must not bring the shell back.
              setSession((s) => (s.state === 'in' && s.me.user.id === next.user.id ? { state: 'in', me: next } : s))
            }
          />
          {leavingGuard}
        </>
      )
    }
  }
}
