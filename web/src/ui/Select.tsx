import type { ComponentPropsWithRef } from 'react'
import { useFieldControl } from './Field'
import { Icon } from './Icon'
import './Input.css'
import './Select.css'

export type SelectProps = Omit<ComponentPropsWithRef<'select'>, 'size' | 'multiple'> & {
  size?: 'md' | 'sm'
  invalid?: boolean
}

export function Select({
  size = 'md',
  invalid,
  className,
  id,
  required,
  'aria-describedby': describedBy,
  'aria-invalid': ariaInvalid,
  children,
  ...rest
}: SelectProps) {
  const { invalid: _, ...field } = useFieldControl({
    id,
    required,
    invalid,
    'aria-describedby': describedBy,
    'aria-invalid': ariaInvalid,
  })
  const cls = ['ui-select', size === 'sm' ? 'ui-select-sm' : '', className ?? ''].filter(Boolean).join(' ')
  return (
    <span className={cls}>
      <select
        className={['ui-control', 'ui-textbox', 'ui-select-native', size === 'sm' ? 'ui-textbox-sm' : 'ui-textbox-md']
          .filter(Boolean)
          .join(' ')}
        {...field}
        {...rest}
      >
        {children}
      </select>
      <Icon name="chevron-down" size={size === 'sm' ? 14 : 16} className="ui-select-chevron" />
    </span>
  )
}
