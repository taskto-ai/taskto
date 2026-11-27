import { type ReactNode, useEffect, useId, useLayoutEffect, useRef } from 'react'
import { createPortal } from 'react-dom'
import { useT } from '../i18n'
import { IconButton } from './IconButton'
import { modalOpen, refocus } from './layers'
import './Drawer.css'

export interface DrawerProps {
  open: boolean
  onClose: () => void
  title: ReactNode
  children: ReactNode
  headerActions?: ReactNode
  footer?: ReactNode
  className?: string
}

const TEXT_INPUT = /^(text|search|email|url|tel|password|number|date|datetime-local|month|time|week)$/
function isField(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false
  if (target.isContentEditable || target instanceof HTMLTextAreaElement || target instanceof HTMLSelectElement)
    return true
  return target instanceof HTMLInputElement && TEXT_INPUT.test(target.type)
}

export function Drawer(props: DrawerProps) {
  if (!props.open) return null
  return createPortal(<OpenDrawer {...props} />, document.body)
}

function OpenDrawer({ onClose, title, children, headerActions, footer, className }: DrawerProps) {
  const t = useT()
  const ref = useRef<HTMLElement>(null)
  const id = useId()
  const close = useRef(onClose)
  useLayoutEffect(() => {
    close.current = onClose
  })

  useLayoutEffect(() => {
    const before = document.activeElement
    ref.current?.focus()
    const drawer = ref.current
    return () => {
      const now = document.activeElement
      if (!now || now === document.body || drawer?.contains(now)) refocus(before)
    }
  }, [])

  useEffect(() => {
    const key = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented || modalOpen() || isField(e.target)) return
      e.preventDefault()
      close.current()
    }
    document.addEventListener('keydown', key)
    return () => document.removeEventListener('keydown', key)
  }, [])

  return (
    <section
      ref={ref}
      className={['ui-drawer', className].filter(Boolean).join(' ')}
      aria-labelledby={`${id}-title`}
      tabIndex={-1}
    >
      <header className="ui-drawer-header">
        <h2 id={`${id}-title`} className="ui-drawer-title">
          {title}
        </h2>
        {headerActions}
        <IconButton icon="close" label={t('ui.close')} onClick={() => close.current()} />
      </header>
      <div className="ui-drawer-body">{children}</div>
      {footer && <footer className="ui-drawer-footer">{footer}</footer>}
    </section>
  )
}
