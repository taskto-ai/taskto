// The session decides the screen: setup while the server has no account, login without a
// session, and the shell with one. The URL is kept through login, so a link still opens
// where it pointed. An invite link (/invite#<token>) opens the invite, with a session or
// without, and then the workspace it joined. A password reset link (/reset-password#<token>)
// sets the password, and signs in.

import { lazy, Suspense, useCallback, useEffect, useLayoutEffect, useState } from 'react'
import {
  ACCOUNT_CHANGED_EVENT,
  ApiError,
  api,
  concernsAccount,
  errorText,
  forAccount,
  type Me,
  SESSION_LOST,
} from './api'
import { InviteScreen, LoginScreen, ResetPasswordScreen, SetupScreen, StartingScreen } from './auth/AuthScreens'
import { ServerUnreachable } from './auth/ServerUnreachable'
import { SigningOut } from './auth/SigningOut'
import { setPreferences, t as translate } from './i18n'
import { leavePush } from './notifications/push'
import { useOpenFromWorker } from './notifications/usePushSync'
import { AccountChanged } from './offline/AccountChanged'
import { onAccountSignal, watchAccount } from './offline/account'
import { ForeignGuard } from './offline/ForeignGuard'
import { useRuntime } from './offline/hooks'
import { LogoutGuard } from './offline/LogoutGuard'
import { recordSignOut, settleSignOut, signOutPending } from './offline/pendingSignOut'
import { activate, discardForeign, lastUser, runtimeState, stillAgreed, type Unsent, unsentOf } from './offline/runtime'
import { approvalOf } from './offline/store'
import { beforeLeaving, navigate } from './router'
import { beginSession, endSession, sameSession, leaving as signingOut } from './session'
import { Shell } from './shell/Shell'
import { cleanUpAfterSignOut, finishUnfinishedSignOuts, SIGN_OUT_WAIT_MS } from './signOut'
import { reportTimeZone } from './timezone'
import { toast } from './ui/Toast'
import { claimDrafts } from './uiState'

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
  // A sign-out that is certain, finishing: nothing of the session is drawn. `back` is where the
  // person was, for when the server didn't confirm it (`failed`) and they go back.
  | { state: 'leaving'; tail: Tail; back: Session; failed?: string }

const INVITE_PATH = '/invite'
const RESET_PATH = '/reset-password'

// Signing out is the same from wherever it is asked: who, and where it leaves to.
type Leaving = { user: number; next: Session }
type Asked = Leaving & Unsent
// What a certain sign-out still has to do, for whom.
type Tail = Leaving & { keep: boolean; approved: ReturnType<typeof approvalOf> }

export function App() {
  const [session, setSession] = useState<Session>({ state: 'loading' })
  const { runtime, foreign, problem } = useRuntime()
  // Unsent drafts kept in this browser belong to whoever is signed in: they are claimed before
  // anything is drawn, so another account never finds the last one's. What is kept for offline use
  // is the same: this account's, and no other's (another account's unsent records are put to the
  // person first, `foreign`). Sending what the account queued starts (or goes on, after the
  // session ended and this person signed in again).
  const claim = useCallback(async (me: Me) => {
    // A sign-out on this device that didn't finish (its tab closed, its cleanup failed) is finished
    // first, with its person's decision, before any account's offline data opens here; one of this
    // account's is not to be, since it is signed in again.
    await finishUnfinishedSignOuts(me.user.id)
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
  // A sign-out whose cleanup on this device didn't finish: said once, on the screen after it (raised
  // once the shell, and the toasts it drew, are gone).
  const [notCleared, setNotCleared] = useState(false)
  useEffect(() => {
    if (!notCleared || session.state === 'leaving' || session.state === 'in') return
    setNotCleared(false)
    toast({ message: translate('app.deviceNotCleared'), tone: 'danger' })
  }, [notCleared, session.state])

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
      if (e instanceof ApiError && e.status === 401) {
        // Nobody is signed in: a sign-out that didn't finish here is finished now.
        await finishUnfinishedSignOuts(null)
        return setSession({ state: 'login' })
      }
      // No answer at all (not a server's error): offline. If someone signed in here, the apps they
      // downloaded can still be used; not those of an account whose sign-out isn't finished (its
      // tab closed on the way): its data is going, and stays closed until a start with the server
      // finishes it.
      const before = !(e instanceof ApiError) ? lastUser() : null
      if (before && !signOutPending(before.id)) {
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
  // Whose the page is: the person signed in, or the one signing out (while it finishes, another tab
  // signing someone else in is heard too).
  const account =
    session.state === 'in' || session.state === 'invite'
      ? (session.me?.user.id ?? null)
      : session.state === 'leaving'
        ? session.tail.user
        : null
  // Another account signed in on this browser, or this one signed out, in another tab: this page's
  // session is over at once (session.ts), and nothing of it is shown until the page is reloaded.
  // Heard here, from the same signals the offline runtime hears, so it holds without a database too
  // (a private window, storage refused); and when the server refuses a request of this account as
  // another's.
  const [changed, setChanged] = useState(false)
  const accountChanged = useCallback(() => {
    endSession()
    setChanged(true)
  }, [])
  useEffect(() => {
    if (account === null) return
    const over = accountChanged
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
  }, [account, accountChanged])
  // The offline runtime found it out first (while activating): the same.
  useLayoutEffect(() => {
    if (problem === 'account') endSession()
  }, [problem])
  const signedInUser = session.state === 'in' ? session.me.user.id : null
  // A click on a push in a window that is open already: heard here, not in the shell, so it isn't
  // lost while the login is showing.
  useOpenFromWorker(session.state === 'in' || session.state === 'login' || session.state === 'loading')
  useEffect(() => {
    if (!signedIn) return
    // The session ended elsewhere (or expired): this device stops receiving the person's pushes.
    const lost = (e: Event) => {
      // An end of the session that names another account than this one's (a deleted account's
      // answer, arriving after this one signed in) leaves this one signed in.
      if (!concernsAccount(e, signedInUser)) return
      void leavePush()
      // What this device kept of an account its person deleted is removed by the page that deleted
      // it (forgetAccount), not here: a session that ends any other way leaves the queue for the
      // next sign-in to send.
      setSession({ state: 'login' })
    }
    window.addEventListener(SESSION_LOST, lost)
    return () => window.removeEventListener(SESSION_LOST, lost)
  }, [signedIn, signedInUser])

  const logIn = useCallback(
    async (me: Me) => {
      await setPreferences({ user: me.user.locale ?? null })
      await signIn(me)
    },
    [signIn],
  )
  // Signing out asks first and ends later. Everything that can stop it is decided while the session
  // goes on (a page with something not saved, records not yet sent): if the person stays, nothing
  // has ended, and what they had on its way (a save, a build) finishes as it would have. Once it is
  // certain, the session ends at once and the shell is no longer drawn (a "signing out" screen), so
  // nothing more can be pressed or sent as the person; the rest (the push subscription, the server,
  // this device's copies) is the tail, bound to this one sign-out (`tail`).

  // The tail, from the session's end: every step after a wait goes on only while no other session
  // has begun here since. The server first: nothing of this device's (the records agreed to lose, the
  // offline database, the drafts, the push subscription) is touched before it has confirmed which
  // session ended. Its answer is waited for a bounded time; no answer, or its own error, leaves the
  // person signed in, as the cookie still is: they are told, and can try again or go back. Once it
  // has confirmed, the cleanup (signOut.ts) acts only on what is still the departing account's, and
  // whatever becomes of it the person reaches the login screen: a cleanup that didn't finish is said
  // once, and tried again at the next start.
  const finish = useCallback(
    async (tail: Tail) => {
      const mine = sameSession()
      const { user, next, keep, approved } = tail
      setSession((s) => (s.state === 'leaving' ? { ...s, failed: undefined } : s))
      // The request says whose session it is for: a tab that is stale (another account signed in
      // since) never signs that account out; the server refuses it.
      const wait = new AbortController()
      const timer = setTimeout(() => wait.abort(), SIGN_OUT_WAIT_MS)
      const outcome = await api('/api/logout', { method: 'POST', headers: forAccount(user), signal: wait.signal }).then(
        () => 'out' as const,
        (e) =>
          e instanceof ApiError && e.code === 'session_account_changed'
            ? ('changed' as const)
            : // A session the server had already ended (elsewhere, or it expired) is signed out.
              e instanceof ApiError && e.status === 401
              ? ('out' as const)
              : wait.signal.aborted
                ? translate('app.signOutNoAnswer')
                : errorText(e),
      )
      clearTimeout(timer)
      if (!mine()) return
      // Refused: the browser's session is another account's now (another tab). Nothing of this one is
      // shown any more; the page is reloaded to go on as whoever that is.
      if (outcome === 'changed') return accountChanged()
      if (outcome !== 'out') {
        setSession((s) => (s.state === 'leaving' ? { ...s, failed: outcome } : s))
        return
      }
      const cleared = await cleanUpAfterSignOut(user, keep, approved)
      if (!mine()) return
      endSession()
      setLeaving(null)
      setSession(next)
      if (!cleared) setNotCleared(true)
    },
    [accountChanged],
  )
  // `keep`: the account's database is not deleted (the person chose so, since this version
  // couldn't tell whether it holds unsent records).
  const signOut = useCallback(
    async ({ user, next, items }: Leaving & Partial<Pick<Unsent, 'items'>>, keep = false) => {
      const still = sameSession()
      const approved = approvalOf(items ?? [])
      // Only asked, nothing changed: if a record that was not shown to the person has appeared since
      // (another tab of the account, a stale one), they are asked again about what is there now.
      // What they agreed to lose is discarded only after the server confirms the sign-out.
      if (!keep) {
        const left = await stillAgreed(user, approved)
        if (!still()) return
        if (left.length > 0) {
          setLeaving({ user, next, count: left.length, items: left, unreadable: null })
          return
        }
      }
      // Certain. The sign-out and its decision are recorded before anything of it waits (the tab may
      // close), the session ends, and the queue sends nothing more for this account; the shell is
      // kept, hidden and inert, in case the server doesn't confirm it and the person goes back.
      recordSignOut(user, keep, approved)
      signingOut(user)
      const queue = runtimeState().runtime
      if (queue?.user === user) queue.engine.pause('signing-out')
      const tail: Tail = { user, next, keep, approved }
      setLeaving(null)
      setSession((s) => ({ state: 'leaving', tail, back: s.state === 'leaving' ? s.back : s }))
      await finish(tail)
    },
    [finish],
  )
  // Every sign-out starts here, and asks two things, each once, in this order. A draft open on the
  // page (a document, a card, a record) first (the router's askers, as for a move to another page):
  // it is what the person sees and was just working on, and keeping it ends there, with nothing else
  // asked. Then the records this device hasn't sent (LogoutGuard), once they mean to go. Cancel at
  // either leaves everything as it was: the session goes on, with the draft and any work in flight.
  const logOut = useCallback(
    (who: Leaving) =>
      beforeLeaving(() => {
        const still = sameSession()
        // A database that can't be checked is not an empty one: the person is told, and chooses.
        void unsentOf(who.user).then((unsent) => {
          if (!still()) return
          if (unsent.count > 0 || unsent.unreadable) setLeaving({ ...who, ...unsent })
          else void signOut(who)
        })
      }),
    [signOut],
  )
  // The server didn't confirm the sign-out and the person goes back. Who the browser is signed in as
  // is asked first: their own shell again only if it is still them (the logout may have gone through
  // before the connection dropped, or another tab signed someone else in meanwhile).
  // Only the same login (the session the shell was for), not only the same account: another login of
  // it made meanwhile is another session, which a page reloaded opens as itself.
  const goBack = useCallback(
    async (back: Session, user: number) => {
      try {
        const me = await api<Me>('/api/me')
        const was = back.state === 'in' || back.state === 'invite' ? back.me?.session : undefined
        if (me.user.id !== user || (was !== undefined && me.session !== was)) return accountChanged()
        // Not signing out after all: nothing of it is to be finished, and the queue goes on.
        settleSignOut(user)
        beginSession(me.user.id)
        const queue = runtimeState().runtime
        if (queue?.user === user && queue.engine.status().paused === 'signing-out') queue.engine.resume()
        // The same state the person left (the shell was kept, with what they were editing).
        setSession(back)
      } catch (e) {
        if (e instanceof ApiError && e.status === 401) return setSession({ state: 'login' })
        setSession((s) => (s.state === 'leaving' ? { ...s, failed: errorText(e) } : s))
      }
    },
    [accountChanged],
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
          onSignOut={() => logOut({ user: me.user.id, next: { state: 'login' } })}
        />
        {leavingGuard}
      </>
    )

  // The signed-in shell, the same element whether the person is in it or signing out from it, so
  // React keeps its state across a sign-out the server didn't confirm.
  const shellFor = (me: Me, out: Extract<Session, { state: 'leaving' }> | null) => (
    <>
      <div hidden={out !== null} inert={out !== null} style={out ? undefined : { display: 'contents' }}>
        <Shell
          me={me}
          onLogout={() => logOut({ user: me.user.id, next: { state: 'login' } })}
          onMe={(next) =>
            // Only the account still signed in: an answer that comes after logout must not bring the shell back.
            setSession((s) => (s.state === 'in' && s.me.user.id === next.user.id ? { state: 'in', me: next } : s))
          }
        />
      </div>
      {out && (
        <SigningOut
          failed={out.failed}
          onRetry={() => void finish(out.tail)}
          onBack={() => void goBack(out.back, out.tail.user)}
        />
      )}
      {leavingGuard}
    </>
  )

  // Another account is signed in on this browser than the one this page holds: nothing of the
  // account's is shown, and the page is reloaded to go on as whoever is signed in.
  if (problem === 'account' || changed) return <AccountChanged />

  switch (session.state) {
    case 'loading':
      return <StartingScreen />
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
            onLogout={() => me && logOut({ user: me.user.id, next: { state: 'invite', me: null } })}
          />
          {leavingGuard}
        </>
      )
    }
    case 'reset':
      return <ResetPasswordScreen token={window.location.hash.slice(1)} onDone={resetDone} />
    case 'error':
      return <ServerUnreachable error={session.error} onRetry={load} />
    case 'leaving':
      // The shell the person signs out from stays mounted, hidden and inert, behind the sign-out's
      // screen: if the server doesn't confirm it and they go back, it is as they left it, with what
      // they were editing. Nothing of it can be pressed meanwhile.
      if (session.back.state === 'in') return shellFor(session.back.me, session)
      return (
        <>
          <SigningOut
            failed={session.failed}
            onRetry={() => void finish(session.tail)}
            onBack={() => void goBack(session.back, session.tail.user)}
          />
          {leavingGuard}
        </>
      )
    case 'offline':
      return (
        <Suspense fallback={<StartingScreen />}>
          <OfflineShell user={session.name} onReconnect={load} />
        </Suspense>
      )
    case 'in': {
      if (foreign) return foreignGuard(session.me)
      return shellFor(session.me, null)
    }
  }
}
