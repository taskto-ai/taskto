// A modal dialog, on the browser's <dialog> and showModal(): it is in the top layer (no ancestor
// clips it), the page behind it is inert and the focus stays inside it. It has a title, a body
// and actions, and a close button.
//
// It is open while it is drawn with `open`: the screen holds the state and closes it in
// `onClose`, which Escape and the close button call. When it opens, the focus goes to the
// element marked data-autofocus, or else the first one that takes it in the body, or else the
// dialog; when it closes, the focus goes back to where it was.

import { type ReactNode, useEffect, useId, useLayoutEffect, useRef } from 'react'
import { useT } from '../i18n'
import { IconButton } from './IconButton'
import { focusables, modalClosed, modalOpened, refocus } from './layers'
import './Dialog.css'

export interface DialogProps {
  open: boolean
  onClose: () => void
  title: ReactNode
  // A line under the title that says what the dialog is for; read with the title.
  description?: ReactNode
  children?: ReactNode
  // The buttons at the end: the action last, Cancel before it.
  actions?: ReactNode
  // `sm` for a question (a confirmation), `md` for a form, `lg` for more.
  size?: 'sm' | 'md' | 'lg'
  // The dialog's role: `alertdialog` for a question that must be answered (ConfirmDialog).
  role?: 'dialog' | 'alertdialog'
  // False for a question that has no way out but its answers (a guard before anything else is
  // shown): no close button, and Escape does nothing. onClose is then never called.
  dismissible?: boolean
}

export function Dialog(props: DialogProps) {
  if (!props.open) return null
  return <OpenDialog {...props} />
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
}: DialogProps) {
  const t = useT()
  const ref = useRef<HTMLDialogElement>(null)
  const id = useId()
  // The latest onClose, for the listeners set up once.
  const close = useRef(onClose)
  close.current = onClose
  const canClose = useRef(dismissible)
  canClose.current = dismissible

  useLayoutEffect(() => {
    const dialog = ref.current
    if (!dialog) return
    const before = document.activeElement
    dialog.showModal()
    modalOpened(dialog)
    // Not the close button, which comes first: what the dialog is for.
    const firstIn = (selector: string) => {
      const part = dialog.querySelector(selector)
      return part ? focusables(part)[0] : undefined
    }
    const first =
      dialog.querySelector<HTMLElement>('[data-autofocus]') ??
      firstIn('.ui-dialog-body') ??
      firstIn('.ui-dialog-actions') ??
      dialog
    first.focus()
    return () => {
      if (dialog.open) dialog.close()
      modalClosed(dialog)
      refocus(before)
    }
  }, [])

  useEffect(() => {
    const dialog = ref.current
    if (!dialog) return
    // Escape: the screen decides (it may be saving).
    const cancel = (e: Event) => {
      e.preventDefault()
      if (canClose.current) close.current()
    }
    // The browser closed it itself (a second Escape it doesn't let a page refuse).
    // A close event comes in a task of its own, so it can be the late echo of a close this
    // component made and has already undone (StrictMode's second mount opens it again): only a
    // dialog that is still closed tells the screen.
    // One that can't be dismissed is opened again: what it guards stays behind it.
    const closed = () => {
      if (dialog.open) return
      if (canClose.current) close.current()
      else if (dialog.isConnected) dialog.showModal()
    }
    dialog.addEventListener('cancel', cancel)
    dialog.addEventListener('close', closed)
    return () => {
      dialog.removeEventListener('cancel', cancel)
      dialog.removeEventListener('close', closed)
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
      // Focus that lands on the dialog itself shows no ring.
      tabIndex={-1}
    >
      {/* Not a <header>: that would be a banner, a landmark of the page, which a dialog's top is not. */}
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
