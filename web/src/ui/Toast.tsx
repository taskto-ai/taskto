// Toasts: a short word that something happened ('Link copied', 'The app was moved'), shown from
// anywhere with toast(), at the window's bottom inline end. They stack (newest last), leave by
// themselves (not while the pointer or the focus is on them), and can be dismissed.
//
//   import { toast } from '../ui/Toast'
//   toast(movedMessage)
//   toast({ message: failedMessage, tone: 'danger', action: { label: retryLabel, onClick: retry } })
//
// They are said by a screen reader through two live regions that are on the page before any
// toast: a polite one, and an assertive one for danger. The stack is a popover in the top layer,
// raised again when a toast comes and when a modal dialog opens, so a toast is above a dialog.
// The first toast() draws the stack in a root of its own if the app hasn't drawn <Toaster />.

import { type ReactNode, useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore } from 'react'
import { createPortal } from 'react-dom'
import { createRoot } from 'react-dom/client'
import { useT } from '../i18n'
import { Button } from './Button'
import { Icon } from './Icon'
import { IconButton } from './IconButton'
import { onModalChange, topModal } from './layers'
import './Toast.css'

export type ToastTone = 'neutral' | 'success' | 'danger'

export interface ToastOptions {
  message: ReactNode
  tone?: ToastTone
  // One button: 'Undo', 'Retry'. It dismisses the toast.
  action?: { label: string; onClick: () => void }
  // How long it stays, in ms. A toast with an action, or a danger one, stays longer.
  duration?: number
}

interface ToastItem extends Required<Omit<ToastOptions, 'action'>> {
  id: number
  action?: ToastOptions['action']
}

// --- The store ----------------------------------------------------------------------------

let items: ToastItem[] = []
let next = 1
const listeners = new Set<() => void>()
let mounted = 0
let raise: (() => void) | null = null

const MAX = 4

function emit() {
  for (const fn of listeners) fn()
}

export function toast(options: ToastOptions | string): number {
  const o = typeof options === 'string' ? { message: options } : options
  const tone = o.tone ?? 'neutral'
  const item: ToastItem = {
    id: next++,
    message: o.message,
    tone,
    action: o.action,
    duration: o.duration ?? (o.action || tone === 'danger' ? 10_000 : 5_000),
  }
  ensureToaster()
  // Above whatever opened since the stack last was.
  raise?.()
  items = [...items, item].slice(-MAX)
  emit()
  return item.id
}

export function dismissToast(id: number): void {
  if (!items.some((i) => i.id === id)) return
  items = items.filter((i) => i.id !== id)
  emit()
}

// For tests: no toast, and the stack's count back to none drawn.
export function resetToasts(): void {
  items = []
  emit()
}

const subscribe = (fn: () => void) => {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

function ensureToaster() {
  if (mounted > 0 || autoHost || typeof document === 'undefined') return
  autoHost = true
  const host = document.createElement('div')
  document.body.append(host)
  createRoot(host).render(<Toaster />)
}
// The stack toast() drew itself, once.
let autoHost = false

// --- The stack ----------------------------------------------------------------------------

// Draw once, near the root of the app, or let the first toast() draw it.
export function Toaster() {
  const t = useT()
  const all = useSyncExternalStore(subscribe, () => items)
  // The live regions are on the page, empty, before the first toast goes in: a region that
  // appears with its words already in it is not always said.
  const [ready, setReady] = useState(false)
  useEffect(() => setReady(true), [])
  const list = ready ? all : []
  const ref = useRef<HTMLElement>(null)
  // Where the stack is in the page: <body>, or the topmost modal dialog while one is open.
  const [host] = useState(() => document.createElement('div'))

  useLayoutEffect(() => {
    mounted++
    const el = ref.current
    // Into the topmost modal dialog (outside it everything is inert, the top layer too), or
    // back to <body>; and on top of the top layer again. A popover that moves is hidden.
    const place = () => {
      const parent = topModal() ?? document.body
      if (host.parentNode !== parent) parent.append(host)
      if (!el?.isConnected) return
      el.hidePopover?.()
      el.showPopover?.()
    }
    place()
    raise = place
    const off = onModalChange(place)
    return () => {
      mounted--
      off()
      if (raise === place) raise = null
      host.remove()
    }
  }, [host])

  return createPortal(
    <section ref={ref} className="ui-toaster" popover="manual" aria-label={t('ui.notifications')}>
      <div className="ui-toaster-region" aria-live="polite" aria-relevant="additions">
        {list
          .filter((i) => i.tone !== 'danger')
          .map((i) => (
            <ToastView key={i.id} item={i} />
          ))}
      </div>
      <div className="ui-toaster-region" aria-live="assertive" aria-relevant="additions">
        {list
          .filter((i) => i.tone === 'danger')
          .map((i) => (
            <ToastView key={i.id} item={i} />
          ))}
      </div>
    </section>,
    host,
  )
}

const ICONS = { neutral: 'info', success: 'check', danger: 'alert' } as const

function ToastView({ item }: { item: ToastItem }) {
  const t = useT()
  const [held, setHeld] = useState(false)

  // It leaves by itself, unless the pointer or the focus is on it: then the time starts again
  // once they leave.
  useEffect(() => {
    if (held) return
    const timer = setTimeout(() => dismissToast(item.id), item.duration)
    return () => clearTimeout(timer)
  }, [held, item.id, item.duration])

  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: hovering only pauses the timer
    <div
      className="ui-toast"
      data-tone={item.tone}
      onPointerEnter={() => setHeld(true)}
      onPointerLeave={() => setHeld(false)}
      onFocus={() => setHeld(true)}
      onBlur={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setHeld(false)
      }}
    >
      <span className="ui-toast-icon" aria-hidden="true">
        <Icon name={ICONS[item.tone]} />
      </span>
      <div className="ui-toast-message">{item.message}</div>
      {item.action && (
        <Button
          variant="ghost"
          size="sm"
          onClick={() => {
            dismissToast(item.id)
            item.action?.onClick()
          }}
        >
          {item.action.label}
        </Button>
      )}
      <IconButton icon="close" size="sm" label={t('ui.dismiss')} onClick={() => dismissToast(item.id)} />
    </div>
  )
}
