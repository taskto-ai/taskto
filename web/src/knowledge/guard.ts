import { createContext, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { navigate } from '../router'

export type Why = 'leave' | 'deleted' | 'moved'

type Wait = { go: () => void; why: Why }

export type Guard = {
  ask: (go: () => void, why?: Why) => void
  report: (editor: string, dirty: boolean, saving: boolean) => void
  saved: (editor: string) => void
  dispose: (editor: string) => void
  forget: () => void
  asking: Why | null
  discard: () => void
  keep: () => void
}

const nothing: Guard = {
  ask: (go) => go(),
  report: () => {},
  saved: () => {},
  dispose: () => {},
  forget: () => {},
  asking: null,
  discard: () => {},
  keep: () => {},
}

export const GuardContext = createContext<Guard>(nothing)

export function useGuard(): Guard {
  const state = useRef({ dirty: false, saving: false })
  const current = useRef<string | null>(null)
  const [pending, setPending] = useState<Wait | null>(null)
  const open = useRef<Wait | null>(null)
  open.current = pending
  const queued = useRef<Wait | null>(null)
  const through = useRef(false)

  const goOn = useCallback(() => {
    const waiting = queued.current
    queued.current = null
    waiting?.go()
  }, [])
  const ask = useCallback((go: () => void, why: Why = 'leave') => {
    if (through.current) go()
    else if (state.current.saving) queued.current = { go, why }
    else if (state.current.dirty) setPending({ go, why })
    else go()
  }, [])
  const report = useCallback(
    (editor: string, dirty: boolean, saving: boolean) => {
      current.current = editor
      state.current = { dirty, saving }
      if (saving) {
        if (open.current) {
          queued.current = open.current
          setPending(null)
        }
      } else if (!dirty) {
        setPending(null)
        goOn()
      } else if (queued.current) {
        setPending(queued.current)
        queued.current = null
      }
    },
    [goOn],
  )
  const saved = useCallback(
    (editor: string) => {
      if (current.current !== editor) return
      setPending(null)
      goOn()
    },
    [goOn],
  )
  const dispose = useCallback((editor: string) => {
    if (current.current !== editor) return
    current.current = null
    state.current = { dirty: false, saving: false }
    queued.current = null
    setPending(null)
  }, [])
  const keep = useCallback(() => setPending(null), [])
  const forget = useCallback(() => {
    queued.current = null
  }, [])
  const discard = useCallback(() => {
    setPending(null)
    through.current = true
    try {
      pending?.go()
    } finally {
      through.current = false
    }
  }, [pending])

  useEffect(() => {
    const unload = (e: BeforeUnloadEvent) => {
      if (!state.current.dirty) return
      e.preventDefault()
      e.returnValue = ''
    }
    const click = (e: MouseEvent) => {
      if (!state.current.dirty || e.defaultPrevented || e.button !== 0) return
      if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
      const a = e.target instanceof Element ? e.target.closest('a[href]') : null
      if (!a || (a.getAttribute('target') ?? '_self') !== '_self') return
      const url = new URL((a as HTMLAnchorElement).href, window.location.href)
      if (url.origin !== window.location.origin || !url.pathname.startsWith('/w/')) return
      if (url.pathname === window.location.pathname) return
      e.preventDefault()
      e.stopPropagation()
      ask(() => navigate(url.pathname))
    }
    window.addEventListener('beforeunload', unload)
    document.addEventListener('click', click, true)
    return () => {
      window.removeEventListener('beforeunload', unload)
      document.removeEventListener('click', click, true)
    }
  }, [ask])

  return useMemo(
    () => ({ ask, report, saved, dispose, forget, asking: pending?.why ?? null, discard, keep }),
    [ask, report, saved, dispose, forget, pending, discard, keep],
  )
}
