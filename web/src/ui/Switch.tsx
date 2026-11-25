// An on/off setting that takes effect at once (a checkbox is for a choice a form sends later): the
// browser's own checkbox with role="switch", so Space turns it and a screen reader says "on" or
// "off". Its label is beside it, before it, as a settings row reads.

import type { ComponentPropsWithRef, ReactNode } from 'react'
import './Switch.css'

export type SwitchProps = Omit<ComponentPropsWithRef<'input'>, 'type' | 'role' | 'children'> & {
  label: ReactNode
}

export function Switch({ label, className, ...rest }: SwitchProps) {
  return (
    <label className={['ui-switch', className ?? ''].filter(Boolean).join(' ')}>
      <span>{label}</span>
      {/* biome-ignore lint/a11y/useAriaPropsForRole: a native checkbox gives the switch its state (checked), as ARIA in HTML says; aria-checked would only repeat it. */}
      <input type="checkbox" role="switch" className="ui-control ui-switch-track" {...rest} />
    </label>
  )
}
