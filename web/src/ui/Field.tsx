// A form field: its label, the control, a hint and an error. The control inside (Input, Textarea,
// Select) takes its id, its description (the hint and the error, through aria-describedby), whether
// it is invalid and whether it is required from the field, so a screen reader says all of it when
// the control is focused. A required field shows a mark after its label; the control itself is
// `required`, which is what a screen reader says.
//
// The error replaces nothing: the hint stays, and the error is said after it. An error is shown
// in words with an icon, never by the red edge alone.

import { createContext, type ReactNode, useContext, useId } from 'react'
import { useT } from '../i18n'
import { Icon } from './Icon'
import './Field.css'

interface FieldState {
  id: string
  describedBy: string | undefined
  invalid: boolean
  required: boolean
}

const FieldContext = createContext<FieldState | null>(null)

export interface FieldProps {
  label: ReactNode
  hint?: ReactNode
  // What is wrong, in words: the field is invalid while it has one.
  error?: ReactNode
  required?: boolean
  // The control's id, when the page needs to know it; one is made otherwise.
  id?: string
  className?: string
  children: ReactNode
}

export function Field({ label, hint, error, required = false, id, className, children }: FieldProps) {
  const t = useT()
  const made = useId()
  const controlId = id ?? made
  const hintId = `${controlId}-hint`
  const errorId = `${controlId}-error`
  const invalid = error !== undefined && error !== null && error !== false && error !== ''
  const describedBy = [hint ? hintId : '', invalid ? errorId : ''].filter(Boolean).join(' ') || undefined
  return (
    <div className={['ui-field', className ?? ''].filter(Boolean).join(' ')} data-invalid={invalid ? '' : undefined}>
      <label className="ui-field-label" htmlFor={controlId}>
        {label}
        {required && (
          <span className="ui-field-required" aria-hidden="true">
            {t('ui.requiredMark')}
          </span>
        )}
      </label>
      <FieldContext.Provider value={{ id: controlId, describedBy, invalid, required }}>
        {children}
      </FieldContext.Provider>
      {hint && (
        <p className="ui-field-hint" id={hintId}>
          {hint}
        </p>
      )}
      {invalid && (
        <p className="ui-field-error" id={errorId}>
          <Icon name="alert" size={14} />
          <span>{error}</span>
        </p>
      )}
    </div>
  )
}

// What a control takes from the field it is in, merged with its own props. In a field the id is
// the field's (give the Field an `id` to choose it), so the label always names the control; the
// control's own description comes before the field's, and its own validity and requirement win.
export function useFieldControl(own: {
  id?: string
  'aria-describedby'?: string
  'aria-invalid'?: boolean | 'true' | 'false' | 'grammar' | 'spelling'
  required?: boolean
  invalid?: boolean
}) {
  const field = useContext(FieldContext)
  const invalid =
    own.invalid ??
    (own['aria-invalid'] !== undefined
      ? own['aria-invalid'] !== 'false' && own['aria-invalid'] !== false
      : (field?.invalid ?? false))
  const describedBy = [own['aria-describedby'], field?.describedBy].filter(Boolean).join(' ') || undefined
  return {
    id: field?.id ?? own.id,
    'aria-describedby': describedBy,
    'aria-invalid': invalid ? (true as const) : undefined,
    required: own.required ?? field?.required ?? undefined,
    invalid,
  }
}
