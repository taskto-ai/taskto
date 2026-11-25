import { useEffect, useRef } from 'react'
import { focusables } from '../ui/layers'

// Whether the focus has dropped to the page's body: the element that held it went (a form closed,
// a card moved to another column and was drawn again there).
export const focusLost = () => {
  const now = document.activeElement
  return !now || now === document.body
}

// The element to give the focus back to when `open` turns false (a form or an editor closes)
// and the focus would otherwise drop to the page's body. Nothing moves when the person has gone on
// somewhere else meanwhile.
export function useFocusBack<T extends HTMLElement>(open: boolean) {
  const ref = useRef<T>(null)
  const was = useRef(open)
  useEffect(() => {
    if (was.current && !open && focusLost()) ref.current?.focus()
    was.current = open
  }, [open])
  return ref
}

// Keeps the focus inside the element `scope` selects (the open card) when what held it goes (a
// form closes, a label is removed) or is disabled while a change is sent, either of which drops
// the focus to the page's body: after each change to its elements, the focus goes back to the element that had
// it once it can take it again, or else to its group's default control (`data-focus-default`) or
// first control (the group is the nearest `data-focus-group` around it, found when it was
// focused), or else to the scope itself. Once the
// person goes elsewhere (the focus or a press lands outside the scope) it is left alone.
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
  // After each change to the scope's elements (one added or removed, one disabled or enabled).
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
