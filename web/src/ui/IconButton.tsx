import type { ComponentPropsWithRef } from 'react'
import { Icon, type IconName } from './Icon'
import { Tooltip } from './Tooltip'
import './IconButton.css'

export type IconButtonProps = Omit<ComponentPropsWithRef<'button'>, 'children' | 'aria-label'> & {
  icon: IconName
  label: string
  size?: 'md' | 'sm'
}

export function IconButton({ icon, label, size = 'md', type = 'button', className, ...rest }: IconButtonProps) {
  const cls = ['ui-control', 'ui-icon-button', size === 'sm' ? 'ui-icon-button-sm' : '', className ?? '']
    .filter(Boolean)
    .join(' ')
  return (
    <Tooltip content={label} describes={false}>
      <button type={type} className={cls} aria-label={label} {...rest}>
        <Icon name={icon} size={size === 'sm' ? 16 : 18} />
      </button>
    </Tooltip>
  )
}
