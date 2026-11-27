import { type ReactNode, useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { useT } from '../i18n'
import { Button } from './Button'
import { Dialog } from './Dialog'
import './ConfirmDialog.css'

export interface ConfirmDialogProps {
  open: boolean
  title: ReactNode
  children?: ReactNode
  confirmLabel: string
  tone?: 'danger' | 'primary'
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
  const cancelButton = useRef<HTMLButtonElement>(null)
  const confirmButton = useRef<HTMLButtonElement>(null)
  useLayoutEffect(() => {
    const now = document.activeElement
    if (busy && (now === cancelButton.current || now === document.body || now === null)) confirmButton.current?.focus()
  }, [busy])
  return (
    <Dialog
      open={open}
      onClose={cancel}
      title={title}
      size="sm"
      role="alertdialog"
      actions={
        <>
          <Button ref={cancelButton} variant="secondary" onClick={cancel} disabled={busy} data-autofocus>
            {cancelLabel ?? t('common.cancel')}
          </Button>
          <Button ref={confirmButton} variant={tone} onClick={onConfirm} loading={busy}>
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

export function useConfirm(): [ReactNode, (options: ConfirmOptions) => Promise<boolean>] {
  const [asking, setAsking] = useState<(ConfirmOptions & { key: number }) | null>(null)
  const asked = useRef(0)
  const answer = useRef<((yes: boolean) => void) | null>(null)

  const confirm = useCallback((options: ConfirmOptions) => {
    answer.current?.(false)
    asked.current += 1
    setAsking({ ...options, key: asked.current })
    return new Promise<boolean>((resolve) => {
      answer.current = resolve
    })
  }, [])

  useEffect(() => () => answer.current?.(false), [])

  const settle = (yes: boolean) => {
    answer.current?.(yes)
    answer.current = null
    setAsking(null)
  }

  const dialog = asking && (
    <ConfirmDialog
      key={asking.key}
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
