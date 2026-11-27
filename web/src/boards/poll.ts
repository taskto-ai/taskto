import { type DependencyList, useEffect, useState } from 'react'
import { ApiError, errorText } from '../api'
import { useMemoBy } from '../memo'

export const POLL_EVERY = 10_000
export const POLL_MAX = 120_000

export function usePoll<T>(
  load: (() => Promise<T>) | null,
  apply: (data: T) => void,
  deps: DependencyList,
  every = POLL_EVERY,
  max = POLL_MAX,
): string | null {
  const [stopped, setStopped] = useState<string | null>(null)
  const run = () => {
    if (!load) return
    let live = true
    let running = false
    let delay = every
    let timer: ReturnType<typeof setTimeout> | undefined
    const schedule = () => {
      clearTimeout(timer)
      if (live && !running && !document.hidden) timer = setTimeout(tick, delay)
    }
    const tick = async () => {
      running = true
      try {
        const data = await load()
        if (!live) return
        apply(data)
        delay = every
      } catch (err) {
        if (!live) return
        if (err instanceof ApiError && [401, 403, 404].includes(err.status)) {
          live = false
          setStopped(errorText(err))
          return
        }
        delay = Math.min(delay * 2, max)
      } finally {
        running = false
      }
      schedule()
    }
    const visibility = () => {
      if (document.hidden) clearTimeout(timer)
      else if (live && !running) {
        clearTimeout(timer)
        void tick()
      }
    }
    setStopped(null)
    document.addEventListener('visibilitychange', visibility)
    schedule()
    return () => {
      live = false
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', visibility)
    }
  }
  const start = useMemoBy(() => run, deps)
  useEffect(start, [start])
  return stopped
}
