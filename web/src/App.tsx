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
import {
  InviteScreen,
  LoginScreen,
  PaymentReturnScreen,
  ResetPasswordScreen,
  SetupScreen,
  StartingScreen,
} from './auth/AuthScreens'
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
import {
  backedSignOut,
  backFromSignOut,
  beginLogout,
  browserLogin,
  confirmSignOut,
  endLogout,
  ensureSignOut,
  forgetLogin,
  loginIsCurrent,
  recordSignOut,
  settleSignOut,
  signOutPending,
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

const OfflineShell = lazy(() => import('./offline/OfflineShell').then((m) => ({ default: m.OfflineShell })))

type Session =
  | { state: 'loading' }
  | { state: 'setup' }
  | { state: 'login' }
  | { state: 'in'; me: Me }
  | { state: 'invite'; me: Me | null }
  | { state: 'reset' }
  | { state: 'payment'; outcome: 'done' | 'cancelled' }
  | { state: 'offline'; name: string }
  | { state: 'error'; error: string }
  | { state: 'leaving'; tail: Tail; failed?: string; busy?: boolean }

const INVITE_PATH = '/invite'
const RESET_PATH = '/reset-password'
const PAYMENT_PATHS: Record<string, 'done' | 'cancelled'> = {
  '/payment/done': 'done',
  '/payment/cancelled': 'cancelled',
}

type Leaving = { user: number; session: string; next: Session }
type Asked = Leaving & Unsent
type Tail = Leaving & Decision

function newerLoginHolds(user: number, session: string): boolean {
  const now = browserLogin()
  return !!session && now?.user === user && now.session !== session
}

export function App() {
  const [session, setSession] = useState<Session>({ state: 'loading' })
  const { runtime, foreign, problem } = useRuntime()
  const claim = useCallback(async (me: Me, asked = Date.now(), still: () => boolean = () => true) => {
    if (!(await finishUnfinishedSignOuts(me, asked))) return false
    if (!still()) return false
    const current = () => still() && loginIsCurrent(me.user.id, me.session)
    if (!current()) return false
    claimDrafts(me.user.id)
    await activate(me.user.id, me.user.name, current)
    if (current()) return true
    await deactivate({ purge: false, user: me.user.id }).catch(() => {})
    return false
  }, [])
  const enter = useCallback(
    async (me: Me, asked?: number, still: () => boolean = () => true) => {
      if (!(await claim(me, asked, still))) return null
      return reportTimeZone(me)
    },
    [claim],
  )
  const [finishedElsewhere, setFinishedElsewhere] = useState(false)
  const staleStart = useRef<() => void>(() => {})
  const signIn = useCallback(
    async (me: Me, asked?: number, language = false) => {
      const now = await enter(me, asked)
      if (!now) return staleStart.current()
      const lost = async () => {
        await deactivate({ purge: false, user: me.user.id }).catch(() => {})
        staleStart.current()
      }
      if (!loginIsCurrent(me.user.id, me.session)) return lost()
      if (language) await setPreferences({ user: now.user.locale ?? null })
      if (!loginIsCurrent(me.user.id, me.session)) return lost()
      beginSession(now.user.id)
      setFinishedElsewhere(false)
      setSession({ state: 'in', me: now })
    },
    [enter],
  )
  const [leaving, setLeaving] = useState<Asked | null>(null)
  const [notCleared, setNotCleared] = useState(false)
  useEffect(() => {
    if (!notCleared || session.state === 'leaving' || session.state === 'in') return
    setNotCleared(false)
    toast({ message: translate('app.deviceNotCleared'), tone: 'danger' })
  }, [notCleared, session.state])
  const over = useCallback(async (tail: Tail, mine: () => boolean) => {
    const { user, session, next } = tail
    confirmSignOut(user, session, tail)
    const cleared = await cleanUpAfterSignOut(user, session, tail)
    if (!mine()) return
    endSession()
    setLeaving(null)
    setSession(next)
    if (!cleared) setNotCleared(true)
  }, [])

  const signedIn = session.state === 'in'
  const account =
    session.state === 'in' || session.state === 'invite'
      ? (session.me?.user.id ?? null)
      : session.state === 'leaving'
        ? session.tail.user
        : null
  const [changed, setChanged] = useState(false)
  const heardChange = useRef(false)
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
  staleStart.current = accountChanged
  const leavingTo = useRef<Session | null>(null)
  leavingTo.current = session.state === 'leaving' ? session.tail.next : null
  useLayoutEffect(() => {
    if (account === null) return
    const over = accountChanged
    watchAccount()
    const off = onAccountSignal((who) => {
      if (who === account) return
      if (who === null && !heardChange.current) forgetOperationsOf(account)
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
    const now = lastUser()
    if (leavingTo.current === null && now !== null && now.id !== account) over()
    return () => {
      off()
      window.removeEventListener(ACCOUNT_CHANGED_EVENT, refused)
    }
  }, [account, accountChanged])
  useLayoutEffect(() => {
    if (problem === 'account') endSession()
  }, [problem])
  const signedInUser = session.state === 'in' ? session.me.user.id : null
  const signedInLogin = session.state === 'in' ? session.me.session : undefined
  useOpenFromWorker(session.state === 'in' || session.state === 'login' || session.state === 'loading')
  useLayoutEffect(() => {
    if (!signedIn) return
    const lost = (e: Event) => {
      if (!concernsAccount(e, signedInUser)) return
      endSession()
      if (signedInUser !== null) forgetLogin(signedInUser, signedInLogin)
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
      if (signedInUser !== null)
        void leavePush(signedInUser, () => {
          const now = browserLogin()
          return !(now?.user === signedInUser && now.session !== signedInLogin)
        })
      setSession({ state: 'login' })
    }
    window.addEventListener(SESSION_LOST, lost)
    return () => window.removeEventListener(SESSION_LOST, lost)
  }, [signedIn, signedInUser, signedInLogin, over, stillHere])

  const logIn = useCallback(
    async (me: Me) => {
      await signIn(me, Date.now(), true)
    },
    [signIn],
  )

  const tailBusy = useRef(false)
  const logoutNow = useCallback(
    async (tail: Tail) => {
      const mine = stillHere()
      const { user, session } = tail
      const before = browserLogin()
      let lapsed = false
      setSession((s) => (s.state === 'leaving' ? { ...s, failed: undefined, busy: false } : s))
      const wait = new AbortController()
      const timer = setTimeout(() => wait.abort(), SIGN_OUT_WAIT_MS)
      if (!newerLoginHolds(user, session)) ensureSignOut(user, session, tail)
      const attempt = beginLogout(user, session)
      let answered = true
      const outcome = await api('/api/logout', {
        method: 'POST',
        headers: forAccount(user),
        ...(session ? { body: { session } } : {}),
        signal: wait.signal,
      }).then(
        () => 'out' as const,
        (e) => {
          if (e instanceof ApiError && e.code === 'session_account_changed') return 'changed' as const
          if (e instanceof ApiError && e.status === 401) {
            lapsed = true
            return 'out' as const
          }
          if (!(e instanceof ApiError) || !e.code) answered = false
          return wait.signal.aborted ? translate('app.signOutNoAnswer') : errorText(e)
        },
      )
      clearTimeout(timer)
      endLogout(attempt, user, session, answered)
      if (outcome === 'out') {
        if (lapsed && before) forgetLogin(before.user, before.session)
        return over(tail, mine)
      }
      if (outcome === 'changed') {
        const now = browserLogin()
        if (now?.user === user && now.session !== session) settleSignOut(user, session)
      }
      if (!mine()) return
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
  const signOut = useCallback(
    async (
      { user, session, next, items, unreadable }: Leaving & Partial<Pick<Unsent, 'items' | 'unreadable'>>,
      keep = false,
    ) => {
      const still = sameSession()
      const approved = approvalOf(items ?? [])
      const discardUnreadable = !keep && !!unreadable
      if (!keep) {
        const left = await stillAgreed(user, approved)
        if (!still()) return
        if (left.length > 0) {
          setLeaving({ user, session, next, count: left.length, items: left, unreadable: null })
          return
        }
      }
      const at = newerLoginHolds(user, session)
        ? Date.now()
        : recordSignOut(user, session, keep, approved, discardUnreadable)
      endSession()
      const queue = runtimeState().runtime
      if (queue?.user === user) queue.engine.pause('signing-out')
      const tail: Tail = { user, session, next, keep, approved, discardUnreadable, at }
      setLeaving(null)
      setSession({ state: 'leaving', tail })
      await finish(tail)
    },
    [finish],
  )
  const logOut = useCallback(
    (who: Leaving) =>
      beforeLeaving(() => {
        const still = sameSession()
        void unsentOf(who.user).then((unsent) => {
          if (!still()) return
          if (unsent.count > 0 || unsent.unreadable) setLeaving({ ...who, ...unsent })
          else void signOut(who)
        })
      }),
    [signOut],
  )
  const backNow = useCallback(
    async (tail: Tail) => {
      const mine = stillHere()
      const before = browserLogin()
      let heard: number | null | undefined
      watchAccount()
      const off = onAccountSignal((who) => {
        heard = who
      })
      try {
        const asked = Date.now()
        const me = await api<Me>('/api/me')
        if (!mine()) return
        if (heard !== undefined || me.user.id !== tail.user || !tail.session || me.session !== tail.session) {
          if (
            (me.user.id === tail.user && tail.session && me.session && me.session !== tail.session) ||
            newerLoginHolds(tail.user, tail.session)
          )
            settleSignOut(tail.user, tail.session)
          return accountChanged()
        }
        const invite = tail.next.state === 'invite'
        const wanted = () => mine() && heard === undefined
        let now: Me | null = me
        if (invite) now = (await claim(me, asked, wanted)) ? me : null
        else now = await enter(me, asked, wanted)
        const holds = () => !!now && mine() && heard === undefined && loginIsCurrent(me.user.id, me.session)
        if (now && holds()) await setPreferences({ user: now.user.locale ?? null })
        if (!now || !holds()) {
          await deactivate({ purge: false, user: me.user.id }).catch(() => {})
          if (mine()) accountChanged()
          return
        }
        backFromSignOut(tail.user, tail.session)
        const queue = runtimeState().runtime
        if (queue?.user === tail.user && queue.engine.status().paused === 'signing-out') queue.engine.resume()
        setLeaving(null)
        beginSession(now.user.id)
        setFinishedElsewhere(false)
        setSession(invite ? { state: 'invite', me: now } : { state: 'in', me: now })
      } catch (e) {
        if (newerLoginHolds(tail.user, tail.session)) settleSignOut(tail.user, tail.session)
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
  const resumed = useCallback(
    async (me: Me, next: Session, asked?: number) => {
      const pending = signOutToResume(me)
      if (!pending) return false
      if (!(await finishUnfinishedSignOuts(me, asked))) {
        staleStart.current()
        return true
      }
      if (!signOutToResume(me)) return false
      await setPreferences({ user: me.user.locale ?? null })
      if (!loginIsCurrent(me.user.id, me.session)) {
        staleStart.current()
        return true
      }
      const still = signOutToResume(me)
      if (!still) return false
      const tail: Tail = { user: still.user, session: still.session, next, ...decisionOf(still) }
      setSession({ state: 'leaving', tail })
      void finish(tail)
      return true
    },
    [finish],
  )
  const load = useCallback(async () => {
    const payment = PAYMENT_PATHS[window.location.pathname]
    if (payment) return setSession({ state: 'payment', outcome: payment })
    let before = browserLogin()
    let started = Date.now()
    const asking = () => {
      before = browserLogin()
      started = Date.now()
      return started
    }
    const lapsed = async () => {
      if (before) forgetLogin(before.user, before.session)
      await finishUnfinishedSignOuts(null, started)
    }
    try {
      const setup = await api<{ needed: boolean }>('/api/setup')
      if (setup.needed) return setSession({ state: 'setup' })
      if (window.location.pathname === RESET_PATH) return setSession({ state: 'reset' })
      if (window.location.pathname === INVITE_PATH) {
        const asked = asking()
        const me = await api<Me>('/api/me').catch((e) => {
          if (e instanceof ApiError && e.status === 401) return null
          throw e
        })
        if (!me) await lapsed()
        if (me && (await resumed(me, { state: 'invite', me: null }, asked))) return
        if (me) {
          if (!(await claim(me, asked))) return staleStart.current()
          beginSession(me.user.id)
        }
        return setSession({ state: 'invite', me })
      }
      const asked = asking()
      const me = await api<Me>('/api/me')
      if (await resumed(me, { state: 'login' }, asked)) return
      await signIn(me, asked, true)
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        await lapsed()
        return setSession({ state: 'login' })
      }
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

  const joined = useCallback(
    (me: Me, before: Me | null) => {
      const known = new Set(before?.workspaces.map((w) => w.id))
      const workspace = me.workspaces.find((w) => !known.has(w.id)) ?? me.workspaces[0]
      navigate(workspace ? `/w/${workspace.id}/conversations` : '/', { replace: true })
      void signIn(me, undefined, true)
    },
    [signIn],
  )
  const resetDone = useCallback(
    (me: Me) => {
      navigate('/', { replace: true })
      void signIn(me, undefined, true)
    },
    [signIn],
  )

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
          failed={foreign.failed}
          onDiscard={() => void discardForeign()}
          onSignOut={() => logOut({ user: me.user.id, session: me.session ?? '', next: { state: 'login' } })}
        />
        {leavingGuard}
      </>
    )

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
    case 'payment':
      return <PaymentReturnScreen outcome={session.outcome} />
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
              setSession((s) => (s.state === 'in' && s.me.user.id === next.user.id ? { state: 'in', me: next } : s))
            }
          />
          {leavingGuard}
        </>
      )
    }
  }
}
