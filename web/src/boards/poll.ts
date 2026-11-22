import { type DependencyList, useCallback, useEffect, useState } from 'react'
import { ApiError, errorText } from '../api'

// How often the open card's thread is asked for again, and the longest wait after failures.
export const POLL_EVERY = 10_000
export const POLL_MAX = 120_000

// Loads with `load` every `every` ms while the page is visible, and hands each answer to
// `apply`. It pauses while the page is hidden and loads at once when it is shown again.
// After a failure it waits twice as long each time, up to `max`, and back to `every` after a
// success. A 401, 403 or 404 stops it for good, since waiting won't change them: the session
// ended, or the caller lost access to what it loads (every request checks access again).
// Answers `stopped`, the error that stopped it, or null.
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
  // biome-ignore lint/correctness/useExhaustiveDependencies: the caller's deps stand for `load` and `apply`
  const start = useCallback(run, deps)
  useEffect(start, [start])
  return stopped
}
