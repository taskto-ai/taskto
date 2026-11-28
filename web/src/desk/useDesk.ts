import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, api, type DeskChanged, type Desk as DeskReply } from '../api'
import { useT } from '../i18n'
import { sameSession } from '../session'
import { toast } from '../ui/Toast'
import { type Change, type Layout, type Reason, readLayout, single } from './layout'

export const SAVE_DELAY_MS = 800

export type DeskState = {
  layout: Layout
  loaded: boolean
  change: (fn: (l: Layout) => Change) => Reason | undefined
}

export function useDesk(workspace: number, path: string): DeskState {
  const t = useT()
  const say = useRef(t)
  say.current = t
  const [same] = useState(sameSession)
  const [layout, setLayout] = useState<Layout>(() => single(path))
  const [loaded, setLoaded] = useState(false)
  const latest = useRef(layout)
  const isLoaded = useRef(false)
  const version = useRef(0)
  const dirty = useRef(false)
  const inflight = useRef(false)
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const alive = useRef(false)
  const flushRef = useRef<(keepalive: boolean) => void>(() => {})
  const base = `/api/workspaces/${workspace}/desks/main`

  const show = useCallback((next: Layout) => {
    latest.current = next
    setLayout(next)
  }, [])

  const flush = useCallback(
    (keepalive: boolean) => {
      clearTimeout(timer.current)
      if (!dirty.current || inflight.current || !same()) return
      dirty.current = false
      inflight.current = true
      let refused = false
      api<DeskReply>(`${base}/layout`, {
        method: 'PUT',
        body: { version: version.current, layout: latest.current },
        keepalive,
      })
        .then((saved) => {
          if (!same()) return
          version.current = saved.version
        })
        .catch((e: unknown) => {
          if (!same()) return
          if (e instanceof ApiError && e.code === 'desk_changed') {
            const current = (e.body as DeskChanged).current
            version.current = current.version
            const theirs = readLayout(current.layout, workspace)
            if (theirs?.root) {
              show(theirs)
              dirty.current = false
              clearTimeout(timer.current)
            } else dirty.current = true
            return
          }
          refused = true
          dirty.current = true
          if (alive.current) toast({ tone: 'danger', message: say.current('desk.saveFailed') })
        })
        .finally(() => {
          inflight.current = false
          if (!same() || !dirty.current || refused) return
          if (alive.current) timer.current = setTimeout(() => flushRef.current(false), SAVE_DELAY_MS)
          else flushRef.current(false)
        })
    },
    [base, same, show, workspace],
  )
  flushRef.current = flush

  useEffect(() => {
    alive.current = true
    const controller = new AbortController()
    api<DeskReply>(base, { signal: controller.signal })
      .then((desk) => {
        if (!alive.current || !same()) return
        version.current = desk.version
        const saved = readLayout(desk.layout, workspace)
        if (saved?.root) show(saved)
        isLoaded.current = true
        setLoaded(true)
      })
      .catch((e: unknown) => {
        if (!alive.current || !same() || controller.signal.aborted) return
        if (e instanceof ApiError || e instanceof TypeError) {
          toast({ tone: 'danger', message: say.current('desk.loadFailed') })
        }
      })
    const leaving = () => flushRef.current(true)
    window.addEventListener('pagehide', leaving)
    return () => {
      window.removeEventListener('pagehide', leaving)
      controller.abort()
      flushRef.current(false)
      alive.current = false
      clearTimeout(timer.current)
      isLoaded.current = false
    }
  }, [base, same, show, workspace])

  const change = useCallback(
    (fn: (l: Layout) => Change): Reason | undefined => {
      const { layout: next, reason } = fn(latest.current)
      if (next === latest.current) return reason
      show(next)
      if (!isLoaded.current) return reason
      dirty.current = true
      clearTimeout(timer.current)
      timer.current = setTimeout(() => flushRef.current(false), SAVE_DELAY_MS)
      return reason
    },
    [show],
  )

  return { layout, loaded, change }
}
