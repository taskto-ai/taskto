import { useCallback, useSyncExternalStore } from 'react'

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
