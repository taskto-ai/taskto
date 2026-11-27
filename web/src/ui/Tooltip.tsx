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
import { focusVisible, listenForInput } from './focusInput'
import './Tooltip.css'

export const OPEN_DELAY_MS = 400
export const CLOSE_DELAY_MS = 120

function spaceToken(name: string): number {
  const v = Number.parseFloat(getComputedStyle(document.documentElement).getPropertyValue(name))
  return Number.isFinite(v) ? v : 8
}

type Describable = ReactElement<{ 'aria-describedby'?: string }>

export interface TooltipProps {
  content: ReactNode
  children: Describable
  describes?: boolean
  side?: 'block' | 'inline-end'
}

function fromOwnPopup(control: Element, from: EventTarget | null): boolean {
  const controlled = control.getAttribute('aria-controls')
  const popup = controlled ? document.getElementById(controlled) : null
  return from instanceof Node && popup !== null && popup.contains(from)
}

function visible(el: HTMLElement): boolean {
  if (typeof el.checkVisibility === 'function') return el.checkVisibility()
  return getComputedStyle(el).display !== 'none'
}

const canPopover = () => typeof HTMLElement !== 'undefined' && 'showPopover' in HTMLElement.prototype

function placeBeside(el: HTMLElement, target: Element, side: NonNullable<TooltipProps['side']>) {
  const gap = spaceToken('--space-2')
  const margin = spaceToken('--space-2')
  const r = target.getBoundingClientRect()
  const w = el.offsetWidth
  const h = el.offsetHeight
  const vw = document.documentElement.clientWidth || window.innerWidth
  const vh = document.documentElement.clientHeight || window.innerHeight
  const clamp = (v: number, size: number, room: number) =>
    Math.min(Math.max(margin, v), Math.max(margin, room - margin - size))
  if (side === 'inline-end') {
    const rtl = getComputedStyle(target).direction === 'rtl'
    const after = rtl ? r.left - gap - w : r.right + gap
    const before = rtl ? r.right + gap : r.left - gap - w
    const fits = (x: number) => x >= margin && x + w <= vw - margin
    if (fits(after) || fits(before)) {
      el.style.top = `${clamp(r.top + r.height / 2 - h / 2, h, vh)}px`
      el.style.left = `${fits(after) ? after : before}px`
      el.dataset.side = 'inline-end'
      return
    }
  }
  const above = r.top - gap - h
  const top = above >= margin || r.bottom + gap + h > vh - margin ? Math.max(margin, above) : r.bottom + gap
  el.style.top = `${top}px`
  el.style.left = `${clamp(r.left + r.width / 2 - w / 2, w, vw)}px`
  delete el.dataset.side
}

export function Tooltip({ content, children, describes = true, side = 'block' }: TooltipProps) {
  const id = useId()
  const anchor = useRef<HTMLSpanElement>(null)
  const tip = useRef<HTMLSpanElement>(null)
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const hovered = useRef(false)
  const focused = useRef(false)
  const [open, setOpen] = useState(false)
  const empty = content === null || content === undefined || content === false || content === ''
  const isEmpty = useRef(empty)
  isEmpty.current = empty

  const show = useCallback((delay: number) => {
    clearTimeout(timer.current)
    if (isEmpty.current) return
    if (delay === 0) setOpen(true)
    else timer.current = setTimeout(() => setOpen(true), delay)
  }, [])
  const hide = useCallback((delay: number) => {
    clearTimeout(timer.current)
    if (delay === 0) setOpen(false)
    else timer.current = setTimeout(() => setOpen(false), delay)
  }, [])
  const release = useCallback(
    (delay: number) => {
      if (!hovered.current && !focused.current) hide(delay)
    },
    [hide],
  )

  useEffect(() => {
    listenForInput()
    return () => clearTimeout(timer.current)
  }, [])

  useEffect(() => {
    if (empty) hide(0)
  }, [empty, hide])

  useEffect(() => {
    if (!open) return
    const key = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      if (tip.current && visible(tip.current)) e.preventDefault()
      hide(0)
    }
    document.addEventListener('keydown', key, true)
    return () => document.removeEventListener('keydown', key, true)
  }, [open, hide])

  useLayoutEffect(() => {
    const el = tip.current
    if (!el || !anchor.current?.firstElementChild || !canPopover()) return
    try {
      if (open) el.showPopover()
      else el.hidePopover()
    } catch {}
  }, [open])

  const place = useCallback(() => {
    const el = tip.current
    const target = anchor.current?.firstElementChild
    if (!el || !target) return
    placeBeside(el, target, side)
  }, [side])
  useLayoutEffect(() => {
    if (open) place()
  })
  useLayoutEffect(() => {
    if (!open) return
    window.addEventListener('scroll', place, { capture: true, passive: true })
    window.addEventListener('resize', place)
    return () => {
      window.removeEventListener('scroll', place, { capture: true })
      window.removeEventListener('resize', place)
    }
  }, [open, place])

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
  const tipIn = () => {
    hovered.current = true
    clearTimeout(timer.current)
  }
  const tipOut = (e: ReactPointerEvent) => {
    if (e.relatedTarget instanceof Node && anchor.current?.contains(e.relatedTarget)) return
    pointerOut(e)
  }

  const focus = (e: ReactFocusEvent) => {
    if (!focusVisible(e.target) || fromOwnPopup(e.target, e.relatedTarget)) return
    focused.current = true
    show(0)
  }
  const blur = () => {
    focused.current = false
    release(0)
  }
  const press = () => {
    hovered.current = false
    focused.current = false
    hide(0)
  }

  const describedBy = [children.props['aria-describedby'], describes && !empty ? id : undefined]
    .filter(Boolean)
    .join(' ')

  return (
    <span
      ref={anchor}
      role="none"
      className="ui-tooltip-anchor"
      onPointerEnter={pointerIn}
      onPointerLeave={pointerOut}
      onPointerDown={press}
      onClick={press}
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
        onPointerLeave={tipOut}
      >
        {content}
      </span>
    </span>
  )
}
