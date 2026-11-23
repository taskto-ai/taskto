// Whether a media query matches, kept up to date: for layouts that change what is drawn (not only
// how), such as the builder's one pane at a time on a phone. Without matchMedia (a test, an old
// browser) nothing matches, so the wide layout is what is drawn.

import { useCallback, useSyncExternalStore } from 'react'

// A phone: the builder shows Build or Preview, one at a time.
export const NARROW = '(max-width: 37.5rem)'

export function useMediaQuery(query: string): boolean {
  const subscribe = useCallback(
    (onChange: () => void) => {
      const list = window.matchMedia?.(query)
      list?.addEventListener?.('change', onChange)
      return () => list?.removeEventListener?.('change', onChange)
    },
    [query],
  )
  return useSyncExternalStore(
    subscribe,
    () => window.matchMedia?.(query).matches ?? false,
    () => false,
  )
}
