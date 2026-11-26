// Toasts: a short word that something happened ('Link copied', 'The app was moved'), shown from
// anywhere with toast(), at the window's bottom inline end. They stack (newest last), leave by
// themselves (not while the pointer or the focus is on them), and can be dismissed.
//
//   import { toast } from '../ui/Toast'
//   toast(movedMessage)
//   toast({ message: failedMessage, tone: 'danger', action: { label: retryLabel, onClick: retry } })
//
// <Toaster /> draws them. The shell draws it once, at the start, so its live regions are on the
// page, empty, before the first toast: a region that appears with its words already in it is not
// always said. Without it, the first toast() draws one in a root of its own. However many are
// drawn, only the first one mounted shows and says anything; the others draw nothing.
//
// What a screen reader says comes from an announcer: two live regions (polite, and assertive for
// danger) that hold a toast's words only for a moment after it comes, and are empty otherwise. The
// toasts you see are not a live region. So the stack can move (into the topmost modal dialog while
// one is open: outside it the page is inert, the top layer too, and back to <body> after) without
// anything being said again. The stack is a popover in the top layer, raised again when a toast
// comes and when a modal dialog opens or reopens, so a toast is above a dialog and usable.

import { type ReactNode, useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore } from 'react'
import { createPortal } from 'react-dom'
import { createRoot } from 'react-dom/client'
import { useT } from '../i18n'
import { onSessionChange } from '../session'
import { Button } from './Button'
import { focusVisible } from './focusInput'
import { Icon } from './Icon'
import { IconButton } from './IconButton'
import { onModalChange, refocus, topModal } from './layers'
import './Toast.css'
import './VisuallyHidden.css'

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

interface State {
  // What is shown.
  items: ToastItem[]
  // What the announcer holds now: each toast for a moment after it comes.
  said: ToastItem[]
  // The mounted Toasters, first first: only it draws.
  owners: symbol[]
}

let state: State = { items: [], said: [], owners: [] }
let next = 1
const listeners = new Set<() => void>()
let raise: (() => void) | null = null

const MAX = 4
// Long enough for a screen reader to have picked the words up, short enough that a stack moved
// into a dialog a moment later carries nothing to say again.
export const ANNOUNCE_MS = 1500

function set(change: Partial<State>) {
  state = { ...state, ...change }
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
  set({ items: [...state.items, item].slice(-MAX), said: [...state.said, item] })
  setTimeout(() => set({ said: state.said.filter((s) => s.id !== item.id) }), ANNOUNCE_MS)
  return item.id
}

export function dismissToast(id: number): void {
  if (!state.items.some((i) => i.id === id)) return
  set({ items: state.items.filter((i) => i.id !== id) })
}

// No toast, nothing being said: when a session begins or ends (session.ts), at that moment, so one
// person's toasts never reach the next, whichever Toaster draws them; and between tests.
export function clearToasts(): void {
  set({ items: [], said: [] })
}
export const resetToasts = clearToasts
onSessionChange(clearToasts)

const subscribe = (fn: () => void) => {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

function ensureToaster() {
  if (state.owners.length > 0 || autoHost || typeof document === 'undefined') return
  autoHost = true
  const root = document.createElement('div')
  root.className = 'ui-toaster-root'
  document.body.append(root)
  createRoot(root).render(<Toaster />)
}
// The stack toast() drew itself, once.
let autoHost = false

// --- The stack ----------------------------------------------------------------------------

// Draw once, near the root of the app (the shell does), or let the first toast() draw it.
export function Toaster() {
  const [me] = useState(() => Symbol('toaster'))
  const first = useSyncExternalStore(subscribe, () => state.owners[0])
  useLayoutEffect(() => {
    set({ owners: [...state.owners, me] })
    return () => set({ owners: state.owners.filter((o) => o !== me) })
  }, [me])
  return first === me ? <Stack /> : null
}

function Stack() {
  const t = useT()
  const { items, said } = useSyncExternalStore(subscribe, () => state)
  // The announcer is on the page, empty, a moment before it holds anything.
  const [ready, setReady] = useState(false)
  useEffect(() => setReady(true), [])
  const ref = useRef<HTMLElement>(null)
  // Where the stack is in the page: <body>, or the topmost modal dialog while one is open.
  const [host] = useState(() => {
    const div = document.createElement('div')
    div.className = 'ui-toaster-host'
    return div
  })

  useLayoutEffect(() => {
    const el = ref.current
    // Into the topmost modal dialog, or back to <body>; and on top of the top layer again. A
    // popover that moves is hidden.
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
      off()
      if (raise === place) raise = null
      host.remove()
    }
  }, [host])

  const saying = ready ? said : []
  return createPortal(
    <>
      <section ref={ref} className="ui-toaster" popover="manual" aria-label={t('ui.notifications')}>
        {items.map((i) => (
          <ToastView key={i.id} item={i} />
        ))}
      </section>
      <div className="ui-visually-hidden ui-toaster-announcer" aria-live="polite" aria-relevant="additions">
        {saying
          .filter((i) => i.tone !== 'danger')
          .map((i) => (
            <p key={i.id} className="ui-toaster-said">
              {i.message}
            </p>
          ))}
      </div>
      <div className="ui-visually-hidden ui-toaster-announcer" aria-live="assertive" aria-relevant="additions">
        {saying
          .filter((i) => i.tone === 'danger')
          .map((i) => (
            <p key={i.id} className="ui-toaster-said">
              {i.message}
            </p>
          ))}
      </div>
    </>,
    host,
  )
}

// Where the focus was in the page when it came into the toasts: it goes back there when the last
// toast it is in goes, whichever toasts it went through.
let cameFrom: Element | null = null

const ICONS = { neutral: 'info', success: 'check', danger: 'alert' } as const

function ToastView({ item }: { item: ToastItem }) {
  const t = useT()
  // The pointer and the focus each hold it, apart: the focus on its action keeps it while the
  // pointer wanders off, and the other way round.
  const [pointer, setPointer] = useState(false)
  const [focus, setFocus] = useState(false)
  const held = pointer || focus
  const ref = useRef<HTMLDivElement>(null)

  // Going with the keyboard's focus in it (its action or its dismiss button pressed, and the
  // action didn't move the focus itself), it doesn't leave the focus to <body>: it goes to the
  // dismiss button of the toast beside this one (never its action, which one more Enter would
  // run), or else back to where it came from. After a press of the pointer, the focus is not
  // the person's place, and is left alone.
  useLayoutEffect(() => {
    const el = ref.current
    return () => {
      const now = document.activeElement
      if (!now || !el?.contains(now) || !focusVisible(now)) return
      const beside = [el.nextElementSibling, el.previousElementSibling].find((n) => n?.classList.contains('ui-toast'))
      const next = beside?.querySelector<HTMLElement>('[data-toast-dismiss]')
      if (next) next.focus()
      else refocus(cameFrom)
    }
  }, [])

  // It leaves by itself, unless the pointer or the focus is on it: then the time starts again
  // once both have left. The focus is asked of the page too when the time is up: one handed to
  // it by a toast that went came while React drew, and React heard no focus event.
  useEffect(() => {
    if (held) return
    const timer = setTimeout(() => {
      if (ref.current?.contains(document.activeElement)) setFocus(true)
      else dismissToast(item.id)
    }, item.duration)
    return () => clearTimeout(timer)
  }, [held, item.id, item.duration])

  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: hovering only pauses the timer
    <div
      ref={ref}
      className="ui-toast"
      data-tone={item.tone}
      onPointerEnter={() => setPointer(true)}
      onPointerLeave={() => setPointer(false)}
      onFocus={(e) => {
        // From the page, not from another toast (one that is going, which hands the focus on).
        const before = e.relatedTarget
        if (!(before instanceof Element && before.closest('.ui-toaster'))) cameFrom = before
        setFocus(true)
      }}
      onBlur={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setFocus(false)
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
      <IconButton
        icon="close"
        size="sm"
        label={t('ui.dismiss')}
        data-toast-dismiss=""
        onClick={() => dismissToast(item.id)}
      />
    </div>
  )
}
