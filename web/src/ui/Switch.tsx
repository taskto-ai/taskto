import type { ComponentPropsWithRef, ReactNode } from 'react'
import './Switch.css'

export type SwitchProps = Omit<ComponentPropsWithRef<'input'>, 'type' | 'role' | 'children'> & {
  label: ReactNode
}

export function Switch({ label, className, ...rest }: SwitchProps) {
  return (
    <label className={['ui-switch', className ?? ''].filter(Boolean).join(' ')}>
      <span>{label}</span>
      <input type="checkbox" role="switch" className="ui-control ui-switch-track" {...rest} />
    </label>
  )
}
