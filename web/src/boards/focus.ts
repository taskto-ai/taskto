import { useEffect, useRef } from 'react'
import { focusables } from '../ui/layers'

export const focusLost = () => {
  const now = document.activeElement
  return !now || now === document.body
}

export function useFocusBack<T extends HTMLElement>(open: boolean) {
  const ref = useRef<T>(null)
  const was = useRef(open)
  useEffect(() => {
    if (was.current && !open && focusLost()) ref.current?.focus()
    was.current = open
  }, [open])
  return ref
}

export function useFocusKeeper(scope: string) {
  const last = useRef<{ el: HTMLElement; group: Element | null } | null>(null)
  useEffect(() => {
    const focused = (e: FocusEvent) => {
      const el = e.target
      last.current =
        el instanceof HTMLElement && el.closest(scope) ? { el, group: el.closest('[data-focus-group]') } : null
    }
    const pressed = (e: PointerEvent) => {
      if (!(e.target instanceof Element && e.target.closest(scope))) last.current = null
    }
    document.addEventListener('focusin', focused)
    document.addEventListener('pointerdown', pressed, true)
    return () => {
      document.removeEventListener('focusin', focused)
      document.removeEventListener('pointerdown', pressed, true)
    }
  }, [scope])
  useEffect(() => {
    const root = document.querySelector<HTMLElement>(scope)
    if (!root) return
    const restore = () => {
      const held = last.current
      if (!held || !focusLost()) return
      if (held.el.isConnected) {
        if (!held.el.matches(':disabled')) held.el.focus()
        return
      }
      const group = held.group?.isConnected ? held.group : null
      const preferred = group?.querySelector<HTMLElement>('[data-focus-default]:not(:disabled)')
      const inGroup = preferred ?? (group ? focusables(group)[0] : undefined)
      ;(inGroup ?? root).focus()
    }
    const observer = new MutationObserver(restore)
    observer.observe(root, { subtree: true, childList: true, attributes: true, attributeFilter: ['disabled'] })
    return () => observer.disconnect()
  }, [scope])
}
