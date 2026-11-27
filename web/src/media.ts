import { type RefObject, useCallback, useContext, useLayoutEffect, useState, useSyncExternalStore } from 'react'
import { PlaceContext } from './router'

export const NARROW = '(max-width: 37.5rem)'

export const NARROW_WIDTH = 600

export const PLACE_ROOT = '[data-place]'

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

function useContainerNarrow(ref: RefObject<Element | null>, on: boolean): boolean {
  const [narrow, setNarrow] = useState(false)
  useLayoutEffect(() => {
    const el = ref.current
    if (!on || !el) return
    const root = el.closest(PLACE_ROOT) ?? el
    const measure = (width: number) => setNarrow(width > 0 && width <= NARROW_WIDTH)
    measure(root.getBoundingClientRect().width)
    if (typeof ResizeObserver === 'undefined') return
    const observer = new ResizeObserver((entries) => {
      const last = entries.at(-1)
      if (last) measure(last.contentRect.width)
    })
    observer.observe(root)
    return () => observer.disconnect()
  }, [ref, on])
  return narrow
}

export function usePageNarrow(ref: RefObject<Element | null>): boolean {
  const inPlace = useContext(PlaceContext) !== null
  const viewport = useMediaQuery(NARROW)
  const container = useContainerNarrow(ref, inPlace)
  return inPlace ? container : viewport
}
