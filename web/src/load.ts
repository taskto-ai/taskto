import { type DependencyList, useCallback, useEffect, useRef, useState } from 'react'
import { errorText } from './api'

export type Loaded<T> = { data: T | undefined; error: string | null; reload: () => void }

// Loads with `load` whenever `deps` change, and on reload(); an answer that arrives after the
// deps changed again is dropped, so a slow reply never shows under the wrong heading.
export function useLoad<T>(load: () => Promise<T>, deps: DependencyList): Loaded<T> {
  const [state, setState] = useState<{ data: T | undefined; error: string | null }>({ data: undefined, error: null })
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
      (data) => live && setState({ data, error: null }),
      // A failed reload keeps what is shown, under the error.
      (e: unknown) => live && setState((s) => ({ data: same ? s.data : undefined, error: errorText(e) })),
    )
    return () => {
      live = false
    }
  }, [run, tick])
  const reload = useCallback(() => setTick((t) => t + 1), [])
  return { ...state, reload }
}
