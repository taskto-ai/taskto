import { type DependencyList, useCallback, useEffect, useRef, useState } from 'react'
import { errorText } from './api'
import { useMemoBy } from './memo'

export type Loaded<T> = {
  data: T | undefined
  error: string | null
  reload: () => void
  settled?: number
  loading?: boolean
}

export function useLoad<T>(load: () => Promise<T>, deps: DependencyList): Loaded<T> {
  const [state, setState] = useState<{ data: T | undefined; error: string | null }>({ data: undefined, error: null })
  const [settled, setSettled] = useState(0)
  const [loading, setLoading] = useState(true)
  const [tick, setTick] = useState(0)
  const run = useMemoBy(() => load, deps)
  const request = useMemoBy(() => ({ run, tick }), [run, tick])
  const last = useRef(run)
  useEffect(() => {
    let live = true
    setLoading(true)
    const same = last.current === request.run
    last.current = request.run
    setState((s) => ({ data: same ? s.data : undefined, error: null }))
    request.run().then(
      (data) => {
        if (!live) return
        setState({ data, error: null })
        setSettled((n) => n + 1)
        setLoading(false)
      },
      (e: unknown) => {
        if (!live) return
        setState((s) => ({ data: same ? s.data : undefined, error: errorText(e) }))
        setSettled((n) => n + 1)
        setLoading(false)
      },
    )
    return () => {
      live = false
    }
  }, [request])
  const reload = useCallback(() => {
    setLoading(true)
    setTick((t) => t + 1)
  }, [])
  return { ...state, reload, settled, loading }
}
