import type { HTMLAttributes, ReactNode } from 'react'
import './Toolbar.css'

export interface ToolbarProps extends HTMLAttributes<HTMLDivElement> {
  label?: string
  children: ReactNode
}

export function Toolbar({ label, className, children, ...rest }: ToolbarProps) {
  return (
    <div
      {...rest}
      className={['ui-toolbar', className].filter(Boolean).join(' ')}
      {...(label ? { role: 'group', 'aria-label': label } : {})}
    >
      {children}
    </div>
  )
}

export function ToolbarSpacer() {
  return <span className="ui-toolbar-spacer" aria-hidden="true" />
}
