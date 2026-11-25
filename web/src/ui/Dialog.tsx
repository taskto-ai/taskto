// A modal dialog, on the browser's <dialog> and showModal(): it is in the top layer (no ancestor
// clips it), the page behind it is inert and the focus stays inside it. It has a title, a body
// and actions, and a close button.
//
// It is open while it is drawn with `open`: the screen holds the state and closes it in
// `onClose`, which Escape and the close button call (and a press outside it, with
// `closeOnOutsidePress`). When it opens, the focus goes to the element marked data-autofocus
// that can take it, or else the first one that takes it in the body, or else in the actions, or
// else the dialog; when it closes, the focus goes back to where it was.
//
// The screen may keep it open (it is saving: ConfirmDialog's `busy`), so Escape only asks. But a
// browser lets a page refuse Escape once per press of the person's, and closes the <dialog> on
// the next one whatever the page says. Then the screen is told, and if it still draws the dialog
// open, the dialog opens again at once, with the focus chosen as on the first opening: what is
// drawn and what the browser shows never part.

import { type MouseEvent, type ReactNode, useEffect, useId, useLayoutEffect, useRef } from 'react'
import { useT } from '../i18n'
import { IconButton } from './IconButton'
import { focusables, modalClosed, modalOpened, modalReopened, refocus } from './layers'
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
  // A press on the dimmed page around it closes it too. Off by default: a form loses nothing to a
  // stray click.
  closeOnOutsidePress?: boolean
}

export function Dialog(props: DialogProps) {
  if (!props.open) return null
  return <OpenDialog {...props} />
}

// Where the focus goes when the dialog opens: not the close button, which comes first, but what
// the dialog is for.
function focusFirst(dialog: HTMLDialogElement) {
  const firstIn = (selector: string) => {
    const part = dialog.querySelector(selector)
    return part ? focusables(part)[0] : undefined
  }
  const marked = [...dialog.querySelectorAll<HTMLElement>('[data-autofocus]')].find(
    (el) => !(el as HTMLButtonElement).disabled,
  )
  const first = marked ?? firstIn('.ui-dialog-body') ?? firstIn('.ui-dialog-actions') ?? dialog
  first.focus()
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
  // The latest props, for the listeners set up once.
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
    // Escape: the screen decides (it may be saving).
    const cancel = (e: Event) => {
      e.preventDefault()
      if (canClose.current) close.current()
    }
    // The browser closed it itself (an Escape it doesn't let a page refuse). A close event comes
    // in a task of its own, so it can be the late echo of a close this component made and has
    // already undone (StrictMode's second mount opens it again): only a dialog that is still
    // closed counts. The screen is told; if it still draws the dialog after it has had its say
    // (this component is still mounted a task later), the dialog opens again. One that can't be
    // dismissed opens again without asking.
    const reopen = () => {
      if (!mounted || dialog.open || !dialog.isConnected) return
      dialog.showModal()
      focusFirst(dialog)
      modalReopened()
    }
    const closed = () => {
      if (dialog.open) return
      clearTimeout(reopening)
      if (!canClose.current) return reopen()
      close.current()
      reopening = setTimeout(reopen)
    }
    dialog.addEventListener('cancel', cancel)
    dialog.addEventListener('close', closed)
    return () => {
      mounted = false
      clearTimeout(reopening)
      dialog.removeEventListener('cancel', cancel)
      dialog.removeEventListener('close', closed)
    }
  }, [])

  // A press on the backdrop is a click on the <dialog> itself, outside its box.
  const pressOutside = (e: MouseEvent<HTMLDialogElement>) => {
    if (!closeOnOutsidePress || !canClose.current || e.target !== e.currentTarget) return
    const r = e.currentTarget.getBoundingClientRect()
    const inside = e.clientX >= r.left && e.clientX <= r.right && e.clientY >= r.top && e.clientY <= r.bottom
    if (!inside) close.current()
  }

  return (
    // biome-ignore lint/a11y/useKeyWithClickEvents: Escape is the keyboard's way out, handled as `cancel`
    <dialog
      ref={ref}
      className="ui-dialog"
      data-size={size}
      role={role === 'alertdialog' ? 'alertdialog' : undefined}
      aria-labelledby={`${id}-title`}
      aria-describedby={description ? `${id}-description` : undefined}
      // Focus that lands on the dialog itself shows no ring.
      tabIndex={-1}
      onClick={pressOutside}
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
