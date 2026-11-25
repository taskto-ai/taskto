import { type DependencyList, useCallback, useEffect, useRef, useState } from 'react'
import { errorText } from './api'

// `settled` counts the loads that have ended, with data or an error: a caller that asks again on
// a timer waits for it to change, so a slow answer is never cut off by the next question.
export type Loaded<T> = { data: T | undefined; error: string | null; reload: () => void; settled?: number }

// Loads with `load` whenever `deps` change, and on reload(); an answer that arrives after the
// deps changed again is dropped, so a slow reply never shows under the wrong heading.
export function useLoad<T>(load: () => Promise<T>, deps: DependencyList): Loaded<T> {
  const [state, setState] = useState<{ data: T | undefined; error: string | null }>({ data: undefined, error: null })
  const [settled, setSettled] = useState(0)
  const [tick, setTick] = useState(0)
  // biome-ignore lint/correctness/useExhaustiveDependencies: the caller's deps stand for `load`
  const run = useCallback(load, deps)
  const last = useRef(run)
  // biome-ignore lint/correctness/useExhaustiveDependencies: a new tick is a reload
  useEffect(() => {
    let live = true
    // A reload keeps what is shown until the answer comes; other deps start empty.
    const same = last.current === run
    last.current = run
    setState((s) => ({ data: same ? s.data : undefined, error: null }))
    run().then(
      (data) => {
        if (!live) return
        setState({ data, error: null })
        setSettled((n) => n + 1)
      },
      // A failed reload keeps what is shown, under the error.
      (e: unknown) => {
        if (!live) return
        setState((s) => ({ data: same ? s.data : undefined, error: errorText(e) }))
        setSettled((n) => n + 1)
      },
    )
    return () => {
      live = false
    }
  }, [run, tick])
  const reload = useCallback(() => setTick((t) => t + 1), [])
  return { ...state, reload, settled }
}
