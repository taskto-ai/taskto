// Not losing a draft: the editor says whether its text differs from what was loaded, and every
// way out of it (Cancel, Back, another document, another space, a link to another page, a
// request that lands and would close or replace it) asks first when it does. Closing the tab
// or leaving the app asks through the browser's own question.
//
// While a save is in flight nothing is asked: the person has already chosen to keep the text.
// What they were doing waits for the save, and goes on when it has landed; if the save failed
// (a conflict), the question is asked then, since the draft is still there.

import { createContext, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { navigate } from '../router'

// Why the question is asked, for what it says: the person left, or the document was deleted or
// moved under the draft.
export type Why = 'leave' | 'deleted' | 'moved'

type Wait = { go: () => void; why: Why }

export type Guard = {
  // Runs `go`: at once if no draft would be lost, else after the question is answered yes.
  ask: (go: () => void, why?: Why) => void
  // What an editor tells the guard, each with its own id: only the editor that was last heard
  // from is listened to, so a save that lands late from one that is gone changes nothing.
  //
  // Whether its draft differs from what was loaded, and whether it is saving.
  report: (editor: string, dirty: boolean, saving: boolean) => void
  // Its save landed, so what was waiting for it goes on.
  saved: (editor: string) => void
  // It is gone, saved or not. The question about its draft goes with it, and so does what
  // waited for a save that never landed: a later choice of the person is not undone.
  dispose: (editor: string) => void
  // The question about discarding the draft, when it is open, and its two answers.
  asking: Why | null
  discard: () => void
  keep: () => void
}

const nothing: Guard = {
  ask: (go) => go(),
  report: () => {},
  saved: () => {},
  dispose: () => {},
  asking: null,
  discard: () => {},
  keep: () => {},
}

export const GuardContext = createContext<Guard>(nothing)

export function useGuard(): Guard {
  const state = useRef({ dirty: false, saving: false })
  // The editor that is there: the one that reported last and has not been disposed.
  const current = useRef<string | null>(null)
  const [pending, setPending] = useState<Wait | null>(null)
  const open = useRef<Wait | null>(null)
  open.current = pending
  const queued = useRef<Wait | null>(null)

  const goOn = useCallback(() => {
    const waiting = queued.current
    queued.current = null
    waiting?.go()
  }, [])
  const ask = useCallback((go: () => void, why: Why = 'leave') => {
    if (state.current.saving) queued.current = { go, why }
    else if (state.current.dirty) setPending({ go, why })
    else go()
  }, [])
  const report = useCallback(
    (editor: string, dirty: boolean, saving: boolean) => {
      current.current = editor
      state.current = { dirty, saving }
      if (saving) {
        // Saving after being asked is an answer: the person keeps the text, and goes on after it.
        if (open.current) {
          queued.current = open.current
          setPending(null)
        }
      } else if (!dirty) {
        // Nothing left to lose (the text was put back, or the save changed nothing).
        setPending(null)
        goOn()
      } else if (queued.current) {
        // The save did not land and the draft is still there: now it is asked.
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
  const discard = useCallback(() => {
    setPending(null)
    pending?.go()
  }, [pending])

  // The page closing or reloading, and a link of the app that would leave this page: both
  // with a draft that differs. (The router has no hook to stop other navigation, such as the
  // browser's Back, so that is not covered.)
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
    () => ({ ask, report, saved, dispose, asking: pending?.why ?? null, discard, keep }),
    [ask, report, saved, dispose, pending, discard, keep],
  )
}
