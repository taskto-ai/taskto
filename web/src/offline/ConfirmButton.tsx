// The inline confirmation the offline screens use until they are rebuilt on the component library
// (its ConfirmDialog). Moved here from the settings, which no longer use it.

import { useState } from 'react'
import { useT } from '../i18n'

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
