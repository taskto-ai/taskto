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
  error?: ReactNode
  required?: boolean
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
      <span className="ui-field-label-row">
        <label className="ui-field-label" htmlFor={controlId}>
          {label}
        </label>
        {required && (
          <span className="ui-field-required" aria-hidden="true">
            {t('ui.requiredMark')}
          </span>
        )}
      </span>
      <FieldContext.Provider value={{ id: controlId, describedBy, invalid, required }}>
        {children}
      </FieldContext.Provider>
      {hint && (
        <p className="ui-field-hint" id={hintId}>
          {hint}
        </p>
      )}
      <div className="ui-field-live" aria-live="polite">
        {invalid && (
          <p className="ui-field-error" id={errorId}>
            <Icon name="alert" size={14} />
            <span>{error}</span>
          </p>
        )}
      </div>
    </div>
  )
}

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
  const token = own.invalid === undefined ? own['aria-invalid'] : undefined
  const kind = token === 'grammar' || token === 'spelling' ? token : true
  return {
    id: field?.id ?? own.id,
    'aria-describedby': describedBy,
    'aria-invalid': invalid ? kind : undefined,
    required: own.required ?? field?.required ?? undefined,
    invalid,
  }
}
