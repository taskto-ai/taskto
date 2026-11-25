// A button that is only an icon. Its label is required: it is the button's accessible name and the
// text of the tooltip that shows on hover and on keyboard focus, so the icon is never the only way
// to learn what the button does.
//
// Two sizes, md (36px) and sm (28px), square with the control's radius; on a touch screen both
// are 44px. A toggle passes aria-pressed and gets the selected look.

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
