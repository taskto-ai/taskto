// A text field of several lines, which the person can make taller. Like Input, it takes its id,
// description, validity and requirement from the Field it is in.

import type { ComponentPropsWithRef } from 'react'
import { useFieldControl } from './Field'
import './Input.css'
import './Textarea.css'

export type TextareaProps = ComponentPropsWithRef<'textarea'> & {
  invalid?: boolean
}

export function Textarea({
  invalid,
  className,
  id,
  required,
  rows = 3,
  'aria-describedby': describedBy,
  'aria-invalid': ariaInvalid,
  ...rest
}: TextareaProps) {
  const { invalid: _, ...field } = useFieldControl({
    id,
    required,
    invalid,
    'aria-describedby': describedBy,
    'aria-invalid': ariaInvalid,
  })
  const cls = ['ui-control', 'ui-textbox', 'ui-textarea', className ?? ''].filter(Boolean).join(' ')
  return <textarea className={cls} rows={rows} {...field} {...rest} />
}
