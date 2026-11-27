import type { ComponentPropsWithRef } from 'react'
import { useFieldControl } from './Field'
import './Input.css'

export type InputProps = Omit<ComponentPropsWithRef<'input'>, 'size'> & {
  size?: 'md' | 'sm'
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
