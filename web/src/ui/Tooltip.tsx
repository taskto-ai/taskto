// A short line of text about a control, shown while the pointer rests on it or the keyboard's focus
// is on it. It is a help, never the only way to learn something a person needs: what a control
// does must be its name or the words beside it.
//
// - It opens after a short rest of a mouse or a pen, and at once on focus from the keyboard. It
//   doesn't open on touch (there is no hover; a tap is a press), nor on a click's focus.
// - The pointer can move onto it without closing it (WCAG 1.4.13); Escape closes it, and so does
//   leaving, blurring or pressing the control.
// - It is in the browser's top layer (a popover), so a dialog or an overflow never clips it, and it
//   is placed above the control, or below when there's no room, inside the window.
// - By default it describes the control (aria-describedby). When its text is the control's own
//   name (an icon button's label), `describes={false}` keeps a screen reader from saying it twice.
//
// The child is one element that takes aria-describedby: a button, a link, a field.

import {
  cloneElement,
  type ReactElement,
  type FocusEvent as ReactFocusEvent,
  type ReactNode,
  type PointerEvent as ReactPointerEvent,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from 'react'
import './Tooltip.css'

// How long the pointer rests before it opens, and how long it may be away (on its way to the
// tooltip) before it closes.
export const OPEN_DELAY_MS = 400
export const CLOSE_DELAY_MS = 120

// The gap between the control and the tooltip, and the margin kept from the window's edges.
const GAP = 6
const MARGIN = 8

type Describable = ReactElement<{ 'aria-describedby'?: string }>

export interface TooltipProps {
  content: ReactNode
  children: Describable
  describes?: boolean
}

const canPopover = () => typeof HTMLElement !== 'undefined' && 'showPopover' in HTMLElement.prototype

// Whether this focus came from the keyboard: the browser's :focus-visible, where it knows it.
function focusVisible(el: Element): boolean {
  try {
    return el.matches(':focus-visible')
  } catch {
    return true
  }
}

export function Tooltip({ content, children, describes = true }: TooltipProps) {
  const id = useId()
  const anchor = useRef<HTMLSpanElement>(null)
  const tip = useRef<HTMLSpanElement>(null)
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  // What holds it open: the pointer resting on the control (or on the tooltip) and the keyboard's
  // focus. Leaving one closes it only when the other is gone too.
  const hovered = useRef(false)
  const focused = useRef(false)
  const [open, setOpen] = useState(false)

  const show = useCallback((delay: number) => {
    clearTimeout(timer.current)
    if (delay === 0) setOpen(true)
    else timer.current = setTimeout(() => setOpen(true), delay)
  }, [])
  const hide = useCallback((delay: number) => {
    clearTimeout(timer.current)
    if (delay === 0) setOpen(false)
    else timer.current = setTimeout(() => setOpen(false), delay)
  }, [])
  // Closes it unless something still holds it open. A hold leaves the timer alone: it can only be
  // an opening on its way (a hold never leaves a closing pending), which must still happen.
  const release = useCallback(
    (delay: number) => {
      if (!hovered.current && !focused.current) hide(delay)
    },
    [hide],
  )

  useEffect(() => () => clearTimeout(timer.current), [])

  // Escape closes it, wherever the focus is, until the control is entered or focused again.
  useEffect(() => {
    if (!open) return
    const key = (e: KeyboardEvent) => {
      if (e.key === 'Escape') hide(0)
    }
    document.addEventListener('keydown', key, true)
    return () => document.removeEventListener('keydown', key, true)
  }, [open, hide])

  // Into the top layer and next to the control, and kept there while the page or any box around
  // the control scrolls, or the window changes size.
  useLayoutEffect(() => {
    const el = tip.current
    const target = anchor.current?.firstElementChild
    if (!el || !target) return
    if (canPopover()) {
      try {
        if (open) el.showPopover()
        else el.hidePopover()
      } catch {
        // Already in the state asked for, or disconnected.
      }
    }
    if (!open) return
    const place = () => {
      const r = target.getBoundingClientRect()
      const w = el.offsetWidth
      const h = el.offsetHeight
      const vw = document.documentElement.clientWidth || window.innerWidth
      const vh = document.documentElement.clientHeight || window.innerHeight
      const above = r.top - GAP - h
      const top = above >= MARGIN || r.bottom + GAP + h > vh - MARGIN ? Math.max(MARGIN, above) : r.bottom + GAP
      const left = Math.min(Math.max(MARGIN, r.left + r.width / 2 - w / 2), Math.max(MARGIN, vw - MARGIN - w))
      el.style.top = `${top}px`
      el.style.left = `${left}px`
    }
    place()
    // Scroll events don't bubble, but a listener capturing on the window hears every box's.
    window.addEventListener('scroll', place, { capture: true, passive: true })
    window.addEventListener('resize', place)
    return () => {
      window.removeEventListener('scroll', place, { capture: true })
      window.removeEventListener('resize', place)
    }
  }, [open])

  const pointerIn = (e: ReactPointerEvent) => {
    if (e.pointerType === 'touch') return
    hovered.current = true
    show(OPEN_DELAY_MS)
  }
  const pointerOut = (e: ReactPointerEvent) => {
    if (e.pointerType === 'touch') return
    hovered.current = false
    release(CLOSE_DELAY_MS)
  }
  // On the tooltip itself: the pointer on its way from the control keeps it open.
  const tipIn = () => {
    hovered.current = true
    clearTimeout(timer.current)
  }
  const focus = (e: ReactFocusEvent) => {
    if (!focusVisible(e.target)) return
    focused.current = true
    show(0)
  }
  const blur = () => {
    focused.current = false
    release(0)
  }
  // Pressing the control is using it: the tooltip goes, whatever held it.
  const press = () => {
    hovered.current = false
    focused.current = false
    hide(0)
  }

  const describedBy = [children.props['aria-describedby'], describes ? id : undefined].filter(Boolean).join(' ')

  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: a box-less wrapper that hears its control's pointer and focus events; the control is what is interactive.
    <span
      ref={anchor}
      className="ui-tooltip-anchor"
      onPointerEnter={pointerIn}
      onPointerLeave={pointerOut}
      onPointerDown={press}
      onFocus={focus}
      onBlur={blur}
    >
      {cloneElement(children, { 'aria-describedby': describedBy || undefined })}
      <span
        ref={tip}
        id={id}
        role="tooltip"
        popover="manual"
        className="ui-tooltip"
        data-open={open ? '' : undefined}
        aria-hidden={describes ? undefined : true}
        onPointerEnter={tipIn}
        onPointerLeave={pointerOut}
      >
        {content}
      </span>
    </span>
  )
}
