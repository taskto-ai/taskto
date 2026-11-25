// A one-line text field (text, email, search, number, password, url …), the height of a button of
// the same size so the two line up in a row. In a Field it takes its id, description, validity and
// requirement from it; alone, it needs an aria-label or a <label> of its own.

import type { ComponentPropsWithRef } from 'react'
import { useFieldControl } from './Field'
import './Input.css'

export type InputProps = Omit<ComponentPropsWithRef<'input'>, 'size'> & {
  size?: 'md' | 'sm'
  // Invalid without a Field around it (a Field's error makes it so already).
  invalid?: boolean
}

export function Input({
  size = 'md',
  invalid,
  className,
  id,
  required,
  'aria-describedby': describedBy,
  'aria-invalid': ariaInvalid,
  ...rest
}: InputProps) {
  const { invalid: _, ...field } = useFieldControl({
    id,
    required,
    invalid,
    'aria-describedby': describedBy,
    'aria-invalid': ariaInvalid,
  })
  const cls = ['ui-control', 'ui-textbox', size === 'sm' ? 'ui-textbox-sm' : 'ui-textbox-md', className ?? '']
    .filter(Boolean)
    .join(' ')
  return <input className={cls} {...field} {...rest} />
}
