// Not losing a draft: the editor says whether its text differs from what was loaded, and every
// way out of it (Cancel, Back, another document, another space) asks first when it does.

import { createContext, useCallback, useMemo, useRef, useState } from 'react'

export type Guard = {
  // Runs `go`: at once if no draft would be lost, else after the question is answered yes.
  ask: (go: () => void) => void
  // The editor's: whether its draft differs from what was loaded. A draft that goes away
  // (saved, or closed) takes the question about it with it.
  report: (dirty: boolean) => void
  // Whether the question about discarding the draft is open, and its two answers.
  asking: boolean
  discard: () => void
  keep: () => void
}

const nothing: Guard = { ask: (go) => go(), report: () => {}, asking: false, discard: () => {}, keep: () => {} }

export const GuardContext = createContext<Guard>(nothing)

export function useGuard(): Guard {
  const dirty = useRef(false)
  const [pending, setPending] = useState<(() => void) | null>(null)
  const ask = useCallback((go: () => void) => {
    if (dirty.current) setPending(() => go)
    else go()
  }, [])
  const report = useCallback((d: boolean) => {
    dirty.current = d
    if (!d) setPending(null)
  }, [])
  const keep = useCallback(() => setPending(null), [])
  const discard = useCallback(() => {
    setPending(null)
    pending?.()
  }, [pending])
  return useMemo(
    () => ({ ask, report, asking: pending !== null, discard, keep }),
    [ask, report, pending, discard, keep],
  )
}
