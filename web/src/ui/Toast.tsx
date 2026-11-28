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
  action?: { label: string; onClick: () => void }
  duration?: number
}

interface ToastItem extends Required<Omit<ToastOptions, 'action'>> {
  id: number
  action?: ToastOptions['action']
}

interface State {
  items: ToastItem[]
  said: ToastItem[]
  owners: symbol[]
}

let state: State = { items: [], said: [], owners: [] }
let next = 1
const listeners = new Set<() => void>()
const announcing = new Map<number, ReturnType<typeof setTimeout>>()
let raise: (() => void) | null = null

const MAX = 4
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
  raise?.()
  set({ items: [...state.items, item].slice(-MAX), said: [...state.said, item] })
  announcing.set(
    item.id,
    setTimeout(() => {
      announcing.delete(item.id)
      set({ said: state.said.filter((s) => s.id !== item.id) })
    }, ANNOUNCE_MS),
  )
  return item.id
}

export function dismissToast(id: number): void {
  if (!state.items.some((i) => i.id === id)) return
  set({ items: state.items.filter((i) => i.id !== id) })
}

export function clearToasts(): void {
  for (const timer of announcing.values()) clearTimeout(timer)
  announcing.clear()
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
let autoHost = false

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
  const [ready, setReady] = useState(false)
  useEffect(() => setReady(true), [])
  const ref = useRef<HTMLElement>(null)
  const [host] = useState(() => {
    const div = document.createElement('div')
    div.className = 'ui-toaster-host'
    return div
  })

  useLayoutEffect(() => {
    const el = ref.current
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

let cameFrom: Element | null = null

const ICONS = { neutral: 'info', success: 'check', danger: 'alert' } as const

function ToastView({ item }: { item: ToastItem }) {
  const t = useT()
  const [pointer, setPointer] = useState(false)
  const [focus, setFocus] = useState(false)
  const held = pointer || focus
  const ref = useRef<HTMLDivElement>(null)

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

  useEffect(() => {
    if (held) return
    const timer = setTimeout(() => {
      if (ref.current?.contains(document.activeElement)) setFocus(true)
      else dismissToast(item.id)
    }, item.duration)
    return () => clearTimeout(timer)
  }, [held, item.id, item.duration])

  return (
    <div
      ref={ref}
      role="none"
      className="ui-toast"
      data-tone={item.tone}
      onPointerEnter={() => setPointer(true)}
      onPointerLeave={() => setPointer(false)}
      onFocus={(e) => {
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
