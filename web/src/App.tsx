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
import { confirmSignOut, recordSignOut, settleSignOut, signOutPending } from './offline/pendingSignOut'
import { activate, discardForeign, lastUser, runtimeState, stillAgreed, type Unsent, unsentOf } from './offline/runtime'
import { approvalOf } from './offline/store'
import { beforeLeaving, navigate } from './router'
import { beginSession, endSession, sameSession } from './session'
import { Shell } from './shell/Shell'
import { cleanUpAfterSignOut, finishUnfinishedSignOuts, SIGN_OUT_WAIT_MS, signOutToResume } from './signOut'
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
type Tail = Leaving & { keep: boolean; approved: ReturnType<typeof approvalOf> }

export function App() {
  const [session, setSession] = useState<Session>({ state: 'loading' })
  const { runtime, foreign, problem } = useRuntime()
  const claim = useCallback(async (me: Me) => {
    await finishUnfinishedSignOuts(me)
    claimDrafts(me.user.id)
    await activate(me.user.id, me.user.name)
  }, [])
  const signIn = useCallback(
    async (me: Me) => {
      await claim(me)
      const now = await reportTimeZone(me)
      beginSession(now.user.id)
      setSession({ state: 'in', me: now })
    },
    [claim],
  )
  const [leaving, setLeaving] = useState<Asked | null>(null)
  const [notCleared, setNotCleared] = useState(false)
  useEffect(() => {
    if (!notCleared || session.state === 'leaving' || session.state === 'in') return
    setNotCleared(false)
    toast({ message: translate('app.deviceNotCleared'), tone: 'danger' })
  }, [notCleared, session.state])

  const signedIn = session.state === 'in'
  const account =
    session.state === 'in' || session.state === 'invite'
      ? (session.me?.user.id ?? null)
      : session.state === 'leaving'
        ? session.tail.user
        : null
  const [changed, setChanged] = useState(false)
  const heardChange = useRef(false)
  const accountChanged = useCallback(() => {
    heardChange.current = true
    endSession()
    setChanged(true)
  }, [])
  const leavingTo = useRef<Session | null>(null)
  leavingTo.current = session.state === 'leaving' ? session.tail.next : null
  useEffect(() => {
    if (account === null) return
    const over = accountChanged
    watchAccount()
    const off = onAccountSignal((who) => {
      if (who === account) return
      if (who === null && !heardChange.current) forgetOperationsOf(account)
      const next = leavingTo.current
      if (who === null && next) {
        endSession()
        setLeaving(null)
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
  useLayoutEffect(() => {
    if (problem === 'account') endSession()
  }, [problem])
  const signedInUser = session.state === 'in' ? session.me.user.id : null
  useOpenFromWorker(session.state === 'in' || session.state === 'login' || session.state === 'loading')
  useEffect(() => {
    if (!signedIn) return
    const lost = (e: Event) => {
      if (!concernsAccount(e, signedInUser)) return
      endSession()
      if (signedInUser !== null) void leavePush(signedInUser)
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

  const tailBusy = useRef(false)
  const over = useCallback(async (tail: Tail, mine: () => boolean) => {
    const { user, next, keep, approved } = tail
    confirmSignOut(user)
    const cleared = await cleanUpAfterSignOut(user, keep, approved)
    if (!mine()) return
    endSession()
    setLeaving(null)
    setSession(next)
    if (!cleared) setNotCleared(true)
  }, [])
  const logoutNow = useCallback(
    async (tail: Tail) => {
      const mine = sameSession()
      const { user, session } = tail
      setSession((s) => (s.state === 'leaving' ? { ...s, failed: undefined, busy: false } : s))
      const wait = new AbortController()
      const timer = setTimeout(() => wait.abort(), SIGN_OUT_WAIT_MS)
      const outcome = await api('/api/logout', {
        method: 'POST',
        headers: forAccount(user),
        ...(session ? { body: { session } } : {}),
        signal: wait.signal,
      }).then(
        () => 'out' as const,
        (e) =>
          e instanceof ApiError && e.code === 'session_account_changed'
            ? ('changed' as const)
            : e instanceof ApiError && e.status === 401
              ? ('out' as const)
              : wait.signal.aborted
                ? translate('app.signOutNoAnswer')
                : errorText(e),
      )
      clearTimeout(timer)
      if (outcome === 'out') return over(tail, mine)
      if (!mine()) return
      if (outcome === 'changed') return accountChanged()
      setSession((s) => (s.state === 'leaving' ? { ...s, failed: outcome } : s))
    },
    [accountChanged, over],
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
    async ({ user, session, next, items }: Leaving & Partial<Pick<Unsent, 'items'>>, keep = false) => {
      const still = sameSession()
      const approved = approvalOf(items ?? [])
      if (!keep) {
        const left = await stillAgreed(user, approved)
        if (!still()) return
        if (left.length > 0) {
          setLeaving({ user, session, next, count: left.length, items: left, unreadable: null })
          return
        }
      }
      recordSignOut(user, session, keep, approved)
      endSession()
      const queue = runtimeState().runtime
      if (queue?.user === user) queue.engine.pause('signing-out')
      const tail: Tail = { user, session, next, keep, approved }
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
      const mine = sameSession()
      let signalled = false
      watchAccount()
      const off = onAccountSignal(() => {
        signalled = true
      })
      try {
        const me = await api<Me>('/api/me')
        off()
        if (!mine()) return
        if (signalled || me.user.id !== tail.user || !tail.session || me.session !== tail.session)
          return accountChanged()
        settleSignOut(tail.user)
        const queue = runtimeState().runtime
        if (queue?.user === tail.user && queue.engine.status().paused === 'signing-out') queue.engine.resume()
        setLeaving(null)
        await setPreferences({ user: me.user.locale ?? null })
        if (tail.next.state === 'invite') {
          await claim(me)
          beginSession(me.user.id)
          setSession({ state: 'invite', me })
        } else {
          await signIn(me)
        }
      } catch (e) {
        off()
        if (!mine()) return
        if (e instanceof ApiError && e.status === 401) return over(tail, mine)
        setSession((s) => (s.state === 'leaving' ? { ...s, failed: errorText(e) } : s))
      } finally {
        off()
      }
    },
    [accountChanged, over, claim, signIn],
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
    async (me: Me, next: Session) => {
      const pending = signOutToResume(me)
      if (!pending) return false
      await finishUnfinishedSignOuts(me)
      const tail: Tail = {
        user: pending.user,
        session: pending.session,
        next,
        keep: pending.keep,
        approved: new Map(pending.approved),
      }
      setSession({ state: 'leaving', tail })
      void finish(tail)
      return true
    },
    [finish],
  )
  const load = useCallback(async () => {
    const payment = PAYMENT_PATHS[window.location.pathname]
    if (payment) return setSession({ state: 'payment', outcome: payment })
    try {
      const setup = await api<{ needed: boolean }>('/api/setup')
      if (setup.needed) return setSession({ state: 'setup' })
      if (window.location.pathname === RESET_PATH) return setSession({ state: 'reset' })
      if (window.location.pathname === INVITE_PATH) {
        const me = await api<Me>('/api/me').catch((e) => {
          if (e instanceof ApiError && e.status === 401) return null
          throw e
        })
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
        await finishUnfinishedSignOuts(null)
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
      void setPreferences({ user: me.user.locale ?? null })
      void signIn(me)
    },
    [signIn],
  )
  const resetDone = useCallback(
    (me: Me) => {
      navigate('/', { replace: true })
      void setPreferences({ user: me.user.locale ?? null })
      void signIn(me)
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
          onDiscard={() => void discardForeign()}
          onSignOut={() => logOut({ user: me.user.id, session: me.session ?? '', next: { state: 'login' } })}
        />
        {leavingGuard}
      </>
    )

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
