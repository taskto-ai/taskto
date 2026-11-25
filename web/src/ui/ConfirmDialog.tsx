// The question before an action that can't be taken back: in place of window.confirm. It says what
// will happen, names the action on its button ("Delete the app", not "OK"), and a destructive
// one is in the danger tone. Cancel takes the focus first, so Enter doesn't destroy by habit.
//
// Drawn by the screen with its state:
//   <ConfirmDialog open={asking} title=… confirmLabel=… tone="danger" onConfirm=… onCancel=…>
//     {what will happen}
//   </ConfirmDialog>
// or asked from an event handler, as window.confirm was:
//   const [confirmDialog, confirm] = useConfirm()
//   …
//   if (await confirm({ title, body, confirmLabel, tone: 'danger' })) remove()
//   …
//   return <>{…}{confirmDialog}</>

import { type ReactNode, useCallback, useEffect, useRef, useState } from 'react'
import { useT } from '../i18n'
import { Button } from './Button'
import { Dialog } from './Dialog'
import './ConfirmDialog.css'

export interface ConfirmDialogProps {
  open: boolean
  title: ReactNode
  // What will happen.
  children?: ReactNode
  // The action, named: "Delete the app".
  confirmLabel: string
  // `danger` for what destroys or can't be undone.
  tone?: 'danger' | 'primary'
  // The action is under way: the button shows it, and nothing closes the dialog.
  busy?: boolean
  onConfirm: () => void
  onCancel: () => void
  cancelLabel?: string
}

export function ConfirmDialog({
  open,
  title,
  children,
  confirmLabel,
  tone = 'primary',
  busy = false,
  onConfirm,
  onCancel,
  cancelLabel,
}: ConfirmDialogProps) {
  const t = useT()
  const cancel = () => {
    if (!busy) onCancel()
  }
  return (
    <Dialog
      open={open}
      onClose={cancel}
      title={title}
      size="sm"
      role="alertdialog"
      actions={
        <>
          <Button variant="secondary" onClick={cancel} disabled={busy} data-autofocus>
            {cancelLabel ?? t('common.cancel')}
          </Button>
          <Button variant={tone} onClick={onConfirm} loading={busy}>
            {confirmLabel}
          </Button>
        </>
      }
    >
      {typeof children === 'string' ? <p className="ui-confirm-text">{children}</p> : children}
    </Dialog>
  )
}

export interface ConfirmOptions {
  title: ReactNode
  body?: ReactNode
  confirmLabel: string
  tone?: 'danger' | 'primary'
  cancelLabel?: string
}

// The dialog to draw, and a function that asks and resolves true when the action is confirmed,
// false when it is cancelled (or the component goes away).
export function useConfirm(): [ReactNode, (options: ConfirmOptions) => Promise<boolean>] {
  const [asking, setAsking] = useState<ConfirmOptions | null>(null)
  const answer = useRef<((yes: boolean) => void) | null>(null)

  const confirm = useCallback((options: ConfirmOptions) => {
    answer.current?.(false)
    setAsking(options)
    return new Promise<boolean>((resolve) => {
      answer.current = resolve
    })
  }, [])

  // A question left open when the component goes is a no.
  useEffect(() => () => answer.current?.(false), [])

  const settle = (yes: boolean) => {
    answer.current?.(yes)
    answer.current = null
    setAsking(null)
  }

  const dialog = asking && (
    <ConfirmDialog
      open
      title={asking.title}
      confirmLabel={asking.confirmLabel}
      tone={asking.tone}
      cancelLabel={asking.cancelLabel}
      onConfirm={() => settle(true)}
      onCancel={() => settle(false)}
    >
      {asking.body}
    </ConfirmDialog>
  )
  return [dialog, confirm]
}
