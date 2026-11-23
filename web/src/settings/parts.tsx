// What the settings pages share: running a change and showing its error, asking before one
// that can't be undone, dates, and copying a value.

import { useCallback, useEffect, useState } from 'react'
import { errorText } from '../api'
import { format, useT } from '../i18n'

// Runs one change at a time; a failure is kept, to show next to what failed.
export function useAction() {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const run = useCallback(async (change: () => Promise<unknown>): Promise<boolean> => {
    setBusy(true)
    setError(null)
    try {
      await change()
      return true
    } catch (e) {
      setError(errorText(e))
      return false
    } finally {
      setBusy(false)
    }
  }, [])
  return { busy, error, run }
}

export function Alert({ error }: { error: string | null | undefined }) {
  if (!error) return null
  return (
    <p role="alert" className="tt-error">
      {error}
    </p>
  )
}

// A button that asks once more before it acts. `name` tells rows apart for screen readers:
// "Remove Alice", not one "Remove" per row.
// `confirm` is the button that acts, in a whole phrase ("Yes, remove"): translated, since
// it can't be made from the label in any language but English.
export function ConfirmButton({
  label,
  name,
  question,
  confirm,
  disabled,
  onOpen,
  onConfirm,
}: {
  label: string
  name?: string
  question: string
  confirm: string
  disabled?: boolean
  // Called when the question is asked, with what the confirmation will be about decided here, not
  // when it is answered: what changed in between is not what was asked about.
  onOpen?: () => void
  onConfirm: () => void
}) {
  const t = useT()
  const [asking, setAsking] = useState(false)
  if (!asking) {
    return (
      <button
        type="button"
        className="tt-button"
        aria-label={name ? t('common.labelFor', { label, name }) : undefined}
        disabled={disabled}
        onClick={() => {
          onOpen?.()
          setAsking(true)
        }}
      >
        {label}
      </button>
    )
  }
  return (
    <fieldset className="tt-confirm" aria-label={question}>
      <span>{question}</span>
      <button
        type="button"
        className="tt-button tt-danger"
        onClick={() => {
          setAsking(false)
          onConfirm()
        }}
      >
        {confirm}
      </button>
      <button type="button" className="tt-button" onClick={() => setAsking(false)}>
        {t('common.cancel')}
      </button>
    </fieldset>
  )
}

// A date, in the user's language.
export function day(time: string): string {
  const d = new Date(time)
  return Number.isNaN(d.getTime()) ? time : format().date(d)
}

// Copies text to the clipboard; without one (plain http, or refused) the text is still selectable.
export function CopyButton({ text, name }: { text: string; name?: string }) {
  const t = useT()
  const [copied, setCopied] = useState(false)
  // "Copied" is for that copy, not from then on; the timer goes with the button.
  useEffect(() => {
    if (!copied) return
    const timer = setTimeout(() => setCopied(false), 2000)
    return () => clearTimeout(timer)
  }, [copied])
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
    } catch {
      // No clipboard (plain http, or refused): the text is selectable.
    }
  }
  return (
    <button
      type="button"
      className="tt-button"
      aria-label={name ? t('common.copyNamed', { name }) : undefined}
      onClick={copy}
    >
      {copied ? t('common.copied') : t('common.copy')}
    </button>
  )
}
