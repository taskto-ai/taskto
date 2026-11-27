import { type RefObject, useLayoutEffect, useState } from 'react'

export const NARROW_WIDTH = 600

export const PAGE_CONTAINER = '[data-page]'

export function useContainerNarrow(ref: RefObject<Element | null>, limit = NARROW_WIDTH): boolean {
  const [narrow, setNarrow] = useState(false)
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const container = el.closest(PAGE_CONTAINER) ?? el
    const measure = (width: number) => setNarrow(width > 0 && width <= limit)
    measure(container.getBoundingClientRect().width)
    if (typeof ResizeObserver === 'undefined') return
    const observer = new ResizeObserver((entries) => {
      const last = entries.at(-1)
      if (last) measure(last.contentRect.width)
    })
    observer.observe(container)
    return () => observer.disconnect()
  }, [ref, limit])
  return narrow
}
