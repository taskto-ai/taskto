import { type ReactNode, useEffect, useId, useLayoutEffect, useRef } from 'react'
import { useT } from '../i18n'
import { IconButton } from './IconButton'
import { focusables, modalClosed, modalOpened, modalReopened, refocus } from './layers'
import './Dialog.css'

export interface DialogProps {
  open: boolean
  onClose: () => void
  title: ReactNode
  description?: ReactNode
  children?: ReactNode
  actions?: ReactNode
  size?: 'sm' | 'md' | 'lg'
  role?: 'dialog' | 'alertdialog'
  dismissible?: boolean
  closeOnOutsidePress?: boolean
}

export function Dialog(props: DialogProps) {
  if (!props.open) return null
  return <OpenDialog {...props} />
}

function focusFirst(dialog: HTMLDialogElement) {
  const within = (selector: string) => {
    const part = dialog.querySelector(selector)
    return part ? focusables(part) : []
  }
  const candidates = [
    ...[...dialog.querySelectorAll<HTMLElement>('[data-autofocus]')],
    ...within('.ui-dialog-body'),
    ...within('.ui-dialog-actions'),
    dialog,
  ]
  for (const el of candidates) {
    el.focus()
    if (document.activeElement === el) return
  }
}

function OpenDialog({
  onClose,
  title,
  description,
  children,
  actions,
  size = 'md',
  role,
  dismissible = true,
  closeOnOutsidePress = false,
}: DialogProps) {
  const t = useT()
  const ref = useRef<HTMLDialogElement>(null)
  const id = useId()
  const close = useRef(onClose)
  const canClose = useRef(dismissible)
  const closesOnOutsidePress = useRef(closeOnOutsidePress)
  useLayoutEffect(() => {
    close.current = onClose
    canClose.current = dismissible
    closesOnOutsidePress.current = closeOnOutsidePress
  })

  useLayoutEffect(() => {
    const dialog = ref.current
    if (!dialog) return
    const before = document.activeElement
    dialog.showModal()
    modalOpened(dialog)
    focusFirst(dialog)
    return () => {
      if (dialog.open) dialog.close()
      modalClosed(dialog)
      refocus(before)
    }
  }, [])

  useEffect(() => {
    const dialog = ref.current
    if (!dialog) return
    let mounted = true
    let reopening: ReturnType<typeof setTimeout> | undefined
    const cancel = (e: Event) => {
      e.preventDefault()
      if (canClose.current) close.current()
    }
    const reopen = () => {
      if (!mounted || dialog.open || !dialog.isConnected) return
      dialog.showModal()
      focusFirst(dialog)
      modalReopened(dialog)
    }
    const closed = () => {
      if (dialog.open) return
      clearTimeout(reopening)
      if (!canClose.current) return reopen()
      close.current()
      reopening = setTimeout(reopen)
    }
    const outside = (e: MouseEvent) => {
      if (e.target !== dialog) return false
      const r = dialog.getBoundingClientRect()
      return !(e.clientX >= r.left && e.clientX <= r.right && e.clientY >= r.top && e.clientY <= r.bottom)
    }
    let pressStartedOutside = false
    const pressDown = (e: MouseEvent) => {
      pressStartedOutside = outside(e)
    }
    const pressOutside = (e: MouseEvent) => {
      const began = pressStartedOutside
      pressStartedOutside = false
      if (!closesOnOutsidePress.current || !canClose.current || !began) return
      if (outside(e)) close.current()
    }
    dialog.addEventListener('cancel', cancel)
    dialog.addEventListener('close', closed)
    dialog.addEventListener('mousedown', pressDown)
    dialog.addEventListener('click', pressOutside)
    return () => {
      mounted = false
      clearTimeout(reopening)
      dialog.removeEventListener('cancel', cancel)
      dialog.removeEventListener('close', closed)
      dialog.removeEventListener('mousedown', pressDown)
      dialog.removeEventListener('click', pressOutside)
    }
  }, [])

  return (
    <dialog
      ref={ref}
      className="ui-dialog"
      data-size={size}
      role={role === 'alertdialog' ? 'alertdialog' : undefined}
      aria-labelledby={`${id}-title`}
      aria-describedby={description ? `${id}-description` : undefined}
      tabIndex={-1}
    >
      <div className="ui-dialog-header">
        <div className="ui-dialog-titles">
          <h2 id={`${id}-title`} className="ui-dialog-title">
            {title}
          </h2>
          {description && (
            <p id={`${id}-description`} className="ui-dialog-description">
              {description}
            </p>
          )}
        </div>
        {dismissible && <IconButton icon="close" label={t('ui.close')} onClick={() => close.current()} />}
      </div>
      {children && <div className="ui-dialog-body">{children}</div>}
      {actions && <footer className="ui-dialog-actions">{actions}</footer>}
    </dialog>
  )
}
