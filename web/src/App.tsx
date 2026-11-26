// The session decides the screen: setup while the server has no account, login without a
// session, and the shell with one. The URL is kept through login, so a link still opens
// where it pointed. An invite link (/invite#<token>) opens the invite, with a session or
// without, and then the workspace it joined. A password reset link (/reset-password#<token>)
// sets the password, and signs in.

import { lazy, Suspense, useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
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
import { forgetOperationsOf } from './apps/operations'
import { InviteScreen, LoginScreen, ResetPasswordScreen, SetupScreen, StartingScreen } from './auth/AuthScreens'
import { ServerUnreachable } from './auth/ServerUnreachable'
import { SigningOut } from './auth/SigningOut'
import { setPreferences, t as translate } from './i18n'
import { leavePush } from './notifications/push'
import { useOpenFromWorker } from './notifications/usePushSync'
import { AccountChanged } from './offline/AccountChanged'
import { forgetLast, onAccountSignal, watchAccount } from './offline/account'
import { ForeignGuard } from './offline/ForeignGuard'
import { useRuntime } from './offline/hooks'
import { LogoutGuard } from './offline/LogoutGuard'
import {
  backedSignOut,
  backFromSignOut,
  browserLogin,
  confirmSignOut,
  forgetLogin,
  recordSignOut,
  signOutPending,
  unansweredSignOut,
} from './offline/pendingSignOut'
import {
  activate,
  deactivate,
  discardForeign,
  lastUser,
  runtimeState,
  stillAgreed,
  type Unsent,
  unsentOf,
} from './offline/runtime'
import { approvalOf } from './offline/store'
import { beforeLeaving, navigate } from './router'
import { beginSession, endSession, sameSession } from './session'
import { Shell } from './shell/Shell'
import {
  cleanUpAfterSignOut,
  type Decision,
  decisionOf,
  finishUnfinishedSignOuts,
  SIGN_OUT_WAIT_MS,
  signOutToResume,
} from './signOut'
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
  // A sign-out that is certain, finishing: nothing of the session is drawn, only this. When the
  // server didn't confirm it (`failed`), the person can try again or go back to the app, which starts
  // afresh for the same login only.
  // `busy`: Back is asking who is signed in; nothing else of the sign-out can be pressed meanwhile.
  | { state: 'leaving'; tail: Tail; failed?: string; busy?: boolean }

const INVITE_PATH = '/invite'
const RESET_PATH = '/reset-password'

// Signing out is the same from wherever it is asked: who, from which login (`session`, as /api/me
// names it), and where it leaves to.
type Leaving = { user: number; session: string; next: Session }
type Asked = Leaving & Unsent
// What a certain sign-out still has to do, for whom, as the person decided.
type Tail = Leaving & Decision

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
    // account's under a newer login is not to be, since the person signed in again (signOut.ts).
    await finishUnfinishedSignOuts(me)
    claimDrafts(me.user.id)
    await activate(me.user.id, me.user.name)
  }, [])
  // Whichever way they signed in, an account with no time zone gets this browser's, once.
  const enter = useCallback(
    async (me: Me) => {
      await claim(me)
      return reportTimeZone(me)
    },
    [claim],
  )
  // Another tab finished this page's own sign-out (see below): the offline runtime heard it as an
  // account change, which it is not for this page. Until someone signs in here again.
  const [finishedElsewhere, setFinishedElsewhere] = useState(false)
  const signIn = useCallback(
    async (me: Me) => {
      const now = await enter(me)
      // The session begins before the shell is drawn, so what the shell starts is in it.
      beginSession(now.user.id)
      setFinishedElsewhere(false)
      setSession({ state: 'in', me: now })
    },
    [enter],
  )
  // Records made on this device that haven't reached the server: asked about before signing out.
  const [leaving, setLeaving] = useState<Asked | null>(null)
  // A sign-out whose cleanup on this device didn't finish: said once, on the screen after it.
  const [notCleared, setNotCleared] = useState(false)
  useEffect(() => {
    if (!notCleared || session.state === 'leaving' || session.state === 'in') return
    setNotCleared(false)
    toast({ message: translate('app.deviceNotCleared'), tone: 'danger' })
  }, [notCleared, session.state])
  // The login is over (the server confirmed it, or said it had already ended): the device is cleared
  // as the person decided, and they reach where the sign-out leads, whatever becomes of the cleanup.
  // Called before this page's session is looked at: a confirmed logout is never left without its
  // cleanup, whatever this page did meanwhile.
  const over = useCallback(async (tail: Tail, mine: () => boolean) => {
    const { user, session, next } = tail
    confirmSignOut(user, session)
    const cleared = await cleanUpAfterSignOut(user, session, tail)
    if (!mine()) return
    endSession()
    setLeaving(null)
    setSession(next)
    if (!cleared) setNotCleared(true)
  }, [])

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
  // Whether this page has heard the browser's account change already: a later "nobody" is then
  // another account's sign-out, not this one's.
  const heardChange = useRef(false)
  // Each time this page moves away from a sign-out it is finishing (another tab finished it, or the
  // account changed): a step of that sign-out still in flight acts no more once this has moved. The
  // session's generation can't say it, since the sign-out ended the session already.
  const page = useRef(0)
  const stillHere = useCallback(() => {
    const mine = sameSession()
    const at = page.current
    return () => mine() && page.current === at
  }, [])
  const accountChanged = useCallback(() => {
    heardChange.current = true
    page.current += 1
    endSession()
    setChanged(true)
  }, [])
  // Where a sign-out this page is finishing leads, read when a signal comes.
  const leavingTo = useRef<Session | null>(null)
  leavingTo.current = session.state === 'leaving' ? session.tail.next : null
  useEffect(() => {
    if (account === null) return
    const over = accountChanged
    watchAccount()
    const off = onAccountSignal((who) => {
      if (who === account) return
      // The account signed out in another tab: what this tab kept of its app submissions waiting
      // for an answer goes too, as in the tab that signed out (each tab keeps its own).
      // Only while nothing else was heard since: then the account that left is this page's.
      if (who === null && !heardChange.current) forgetOperationsOf(account)
      // Nobody signed in, while this page signs the account out: another tab finished the same
      // sign-out (its cleanup forgot the account). Not another account: on to where it leads.
      const next = leavingTo.current
      if (who === null && next) {
        page.current += 1
        endSession()
        setLeaving(null)
        setFinishedElsewhere(true)
        setSession(next)
        return
      }
      over()
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
  const signedInLogin = session.state === 'in' ? session.me.session : undefined
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
      // Ended here too, not only by a refused request: the deletion's answer says so without one.
      endSession()
      // This login is over: it shields nothing any more (signOut.ts, a late cleanup of an older one).
      if (signedInUser !== null) forgetLogin(signedInUser, signedInLogin)
      // The person chose to sign out, got no answer, and went back: that logout may be what ended the
      // login now. It is finished as they decided (the records they agreed to lose go), never left
      // for the next sign-in to send.
      const backed = signedInUser !== null ? backedSignOut(signedInUser, signedInLogin) : undefined
      if (backed) {
        const tail: Tail = {
          user: backed.user,
          session: backed.session,
          next: { state: 'login' },
          ...decisionOf(backed),
        }
        setSession({ state: 'leaving', tail })
        void over(tail, stillHere())
        return
      }
      if (signedInUser !== null) void leavePush(signedInUser)
      // What this device kept of an account its person deleted is removed by the page that deleted
      // it (forgetAccount), not here: a session that ends any other way leaves the queue for the
      // next sign-in to send.
      setSession({ state: 'login' })
    }
    window.addEventListener(SESSION_LOST, lost)
    return () => window.removeEventListener(SESSION_LOST, lost)
  }, [signedIn, signedInUser, signedInLogin, over, stillHere])

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
  // Only one step of the tail is in flight at a time: the logout (the first, or Try again) or Back's
  // question, and each is refused while the other is in flight (`tailBusy`), even pressed in the same
  // moment. A logout that got no answer may still reach the server after Back found the login going
  // on: Back then keeps the sign-out recorded (`backed`, pendingSignOut.ts), and if that login ends
  // while it is, the sign-out is finished as the person decided (the session-lost handler above).
  const tailBusy = useRef(false)
  const logoutNow = useCallback(
    async (tail: Tail) => {
      const mine = stillHere()
      const { user, session } = tail
      // The login the browser's cookie is (the last one started here): a 401 says it is over.
      const before = browserLogin()
      let lapsed = false
      setSession((s) => (s.state === 'leaving' ? { ...s, failed: undefined, busy: false } : s))
      // The request says whose session it is for, and which login: a tab that is stale (another
      // account signed in since, or this one again with a newer login) never signs that out; the
      // server refuses it.
      const wait = new AbortController()
      const timer = setTimeout(() => wait.abort(), SIGN_OUT_WAIT_MS)
      const outcome = await api('/api/logout', {
        method: 'POST',
        headers: forAccount(user),
        ...(session ? { body: { session } } : {}),
        signal: wait.signal,
      }).then(
        () => 'out' as const,
        (e) => {
          if (e instanceof ApiError && e.code === 'session_account_changed') return 'changed' as const
          // A session the server had already ended (elsewhere, or it expired) is signed out.
          if (e instanceof ApiError && e.status === 401) {
            lapsed = true
            return 'out' as const
          }
          // No answer from the server itself (none in time, the connection, a proxy's page): the
          // request may still reach it.
          if (!(e instanceof ApiError) || !e.code) unansweredSignOut(user, session)
          return wait.signal.aborted ? translate('app.signOutNoAnswer') : errorText(e)
        },
      )
      clearTimeout(timer)
      // Confirmed: the device is cleared as the person decided, whatever this page shows by now. When
      // the browser's login had ended already, it shields nothing (a newer one that is over too).
      if (outcome === 'out') {
        if (lapsed && before) forgetLogin(before.user, before.session)
        return over(tail, mine)
      }
      if (!mine()) return
      // Refused: the browser's login is another one now (another tab). Nothing of this one is shown
      // any more; the page is reloaded to go on as whoever that is.
      if (outcome === 'changed') return accountChanged()
      setSession((s) => (s.state === 'leaving' ? { ...s, failed: outcome } : s))
    },
    [accountChanged, over, stillHere],
  )
  const finish = useCallback(
    async (tail: Tail) => {
      if (tailBusy.current) return
      tailBusy.current = true
      try {
        await logoutNow(tail)
      } finally {
        tailBusy.current = false
      }
    },
    [logoutNow],
  )
  // `keep`: the account's database is not deleted (the person chose so, since this version
  // couldn't tell whether it holds unsent records).
  // `unreadable`: the person was told their database couldn't be read; without `keep`, they chose
  // to discard it.
  const signOut = useCallback(
    async (
      { user, session, next, items, unreadable }: Leaving & Partial<Pick<Unsent, 'items' | 'unreadable'>>,
      keep = false,
    ) => {
      const still = sameSession()
      const approved = approvalOf(items ?? [])
      const discardUnreadable = !keep && !!unreadable
      // Only asked, nothing changed: if a record that was not shown to the person has appeared since
      // (another tab of the account, a stale one), they are asked again about what is there now.
      // What they agreed to lose is discarded only after the server confirms the sign-out.
      if (!keep) {
        const left = await stillAgreed(user, approved)
        if (!still()) return
        if (left.length > 0) {
          setLeaving({ user, session, next, count: left.length, items: left, unreadable: null })
          return
        }
      }
      // Certain. The sign-out, its login and its decision are recorded before anything of it waits
      // (the tab may close), the session ends, the queue sends nothing more for this account (no tab's
      // does, while the record is there), and the shell goes: nothing of it is kept for a sign-out the
      // server doesn't confirm, whose Back starts the app afresh.
      recordSignOut(user, session, keep, approved, discardUnreadable)
      endSession()
      const queue = runtimeState().runtime
      if (queue?.user === user) queue.engine.pause('signing-out')
      const tail: Tail = { user, session, next, keep, approved, discardUnreadable }
      setLeaving(null)
      setSession({ state: 'leaving', tail })
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
  // is asked first, and the app starts afresh only for the same login the sign-out was for (not only
  // the same account: another login of it is another session, which a page reloaded opens as
  // itself). An answer during which another tab signed in (any account, this one too) is not taken:
  // it may describe the login from before. A login that is over (401) is a sign-out done.
  const backNow = useCallback(
    async (tail: Tail) => {
      const mine = stillHere()
      const before = browserLogin()
      // What other tabs say while this goes on, up to its last step: the account changed (another
      // person signed in), or nobody is signed in (another tab finished this sign-out). Undefined
      // while nothing was heard.
      let heard: number | null | undefined
      watchAccount()
      const off = onAccountSignal((who) => {
        heard = who
      })
      try {
        const me = await api<Me>('/api/me')
        if (!mine()) return
        if (heard !== undefined || me.user.id !== tail.user || !tail.session || me.session !== tail.session)
          return accountChanged()
        // Not signing out after all. The app starts as it does on a fresh start (the account's
        // offline data, drafts and submissions claimed, its time zone reported), and only once that
        // has worked is the sign-out let go and the queue sent again: a step that fails leaves the
        // person on "Couldn't sign out", with the sign-out as it was. Asked again after every wait:
        // nothing of the account is claimed once another tab finished the sign-out or someone else
        // signed in, and what was claimed is let go if that came while it was.
        await setPreferences({ user: me.user.locale ?? null })
        if (!mine()) return
        if (heard !== undefined) return accountChanged()
        const invite = tail.next.state === 'invite'
        let now = me
        if (invite) await claim(me)
        else now = await enter(me)
        if (!mine() || heard !== undefined) {
          await deactivate({ purge: false, user: me.user.id }).catch(() => {})
          if (heard === null) forgetLast(me.user.id)
          if (mine()) accountChanged()
          return
        }
        backFromSignOut(tail.user, tail.session)
        const queue = runtimeState().runtime
        if (queue?.user === tail.user && queue.engine.status().paused === 'signing-out') queue.engine.resume()
        setLeaving(null)
        // In a new session, begun before anything of it is drawn.
        beginSession(now.user.id)
        setFinishedElsewhere(false)
        setSession(invite ? { state: 'invite', me: now } : { state: 'in', me: now })
      } catch (e) {
        // A login that is over is a sign-out done, whatever this page did meanwhile; it shields
        // nothing any more.
        if (e instanceof ApiError && e.status === 401) {
          if (before) forgetLogin(before.user, before.session)
          return over(tail, mine)
        }
        if (!mine()) return
        setSession((s) => (s.state === 'leaving' ? { ...s, failed: errorText(e) } : s))
      } finally {
        off()
      }
    },
    [accountChanged, over, claim, enter, stillHere],
  )
  const goBack = useCallback(
    async (tail: Tail) => {
      if (tailBusy.current) return
      tailBusy.current = true
      setSession((s) => (s.state === 'leaving' ? { ...s, busy: true } : s))
      try {
        await backNow(tail)
      } finally {
        tailBusy.current = false
        setSession((s) => (s.state === 'leaving' ? { ...s, busy: false } : s))
      }
    },
    [backNow],
  )
  // This login's sign-out didn't finish (its tab closed, or this is another tab of it): it goes on,
  // as its person decided, rather than signing them in again. False when there is none.
  const resumed = useCallback(
    async (me: Me, next: Session) => {
      const pending = signOutToResume(me)
      if (!pending) return false
      await finishUnfinishedSignOuts(me)
      const tail: Tail = { user: pending.user, session: pending.session, next, ...decisionOf(pending) }
      setSession({ state: 'leaving', tail })
      void finish(tail)
      return true
    },
    [finish],
  )
  const load = useCallback(async () => {
    // The login the browser's cookie is, as far as this browser knows: a 401 says it is over.
    const before = browserLogin()
    const lapsed = async () => {
      if (before) forgetLogin(before.user, before.session)
      // Nobody is signed in: a sign-out that didn't finish here is finished now.
      await finishUnfinishedSignOuts(null)
    }
    try {
      const setup = await api<{ needed: boolean }>('/api/setup')
      if (setup.needed) return setSession({ state: 'setup' })
      if (window.location.pathname === RESET_PATH) return setSession({ state: 'reset' })
      if (window.location.pathname === INVITE_PATH) {
        const me = await api<Me>('/api/me').catch((e) => {
          if (e instanceof ApiError && e.status === 401) return null
          throw e
        })
        if (!me) await lapsed()
        // Signed in on the invite page is signed in: what this device keeps of the account is
        // theirs from here, and is what signing out from the page takes away.
        if (me && (await resumed(me, { state: 'invite', me: null }))) return
        if (me) {
          await claim(me)
          beginSession(me.user.id)
        }
        return setSession({ state: 'invite', me })
      }
      const me = await api<Me>('/api/me')
      await setPreferences({ user: me.user.locale ?? null })
      if (await resumed(me, { state: 'login' })) return
      await signIn(me)
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        await lapsed()
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
  }, [signIn, claim, resumed])

  useEffect(() => {
    load()
  }, [load])

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
          onSignOut={() => logOut({ user: me.user.id, session: me.session ?? '', next: { state: 'login' } })}
        />
        {leavingGuard}
      </>
    )

  // Another account is signed in on this browser than the one this page holds: nothing of the
  // account's is shown, and the page is reloaded to go on as whoever is signed in.
  if ((problem === 'account' && !finishedElsewhere) || changed) return <AccountChanged />

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
            onLogout={() =>
              me && logOut({ user: me.user.id, session: me.session ?? '', next: { state: 'invite', me: null } })
            }
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
      return (
        <SigningOut
          failed={session.failed}
          busy={session.busy}
          onRetry={() => void finish(session.tail)}
          onBack={() => void goBack(session.tail)}
        />
      )
    case 'offline':
      return (
        <Suspense fallback={<StartingScreen />}>
          <OfflineShell user={session.name} onReconnect={load} />
        </Suspense>
      )
    case 'in': {
      if (foreign) return foreignGuard(session.me)
      const me = session.me
      return (
        <>
          <Shell
            me={me}
            onLogout={() => logOut({ user: me.user.id, session: me.session ?? '', next: { state: 'login' } })}
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
