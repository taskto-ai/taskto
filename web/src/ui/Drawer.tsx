// A side panel over the page: a card's detail over its board. It is not modal: the page beside it
// stays usable, and it is a labelled region (its title names it). It sits at the window's inline
// end, at --z-drawer (above the sidebar, under a menu, a dialog and the toasts), drawn into
// <body> so no scrolling or clipped ancestor cuts it; on a phone it takes the screen.
//
// It is open while it is drawn with `open`. Escape closes it, as does its close button; but not
// when a menu or a dialog over it took the key, and not when the focus is in a field (a text box,
// a select, editable text), in the drawer or in the page: there Escape belongs to the field (it
// clears a search, closes a picker, undoes an edit). Tab out of the field and press it again. When it opens the focus goes into it; when it closes
// with the focus inside, the focus goes back to where it was before.

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
  // Buttons beside the close button: a "more" menu.
  headerActions?: ReactNode
  // A bar at the bottom: the main actions, a composer.
  footer?: ReactNode
  // A class of the screen's own on the drawer, for its layout (where it takes the whole window).
  className?: string
}

// A field that may have a use for Escape of its own.
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
  close.current = onClose

  useLayoutEffect(() => {
    const before = document.activeElement
    ref.current?.focus()
    const drawer = ref.current
    return () => {
      // Back to the opener only if the focus was in the drawer (or went with it): a person who
      // went on in the page keeps their place.
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
