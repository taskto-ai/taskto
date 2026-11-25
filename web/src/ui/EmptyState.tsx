// What a place shows when it has nothing yet: a line that says what this place is for, a line
// more if it helps, and the action to take (a button or two).

import type { ReactNode } from 'react'
import './EmptyState.css'

export interface EmptyStateProps {
  // What this place is for, or that there is nothing here yet: "No apps in Product team yet".
  title: ReactNode
  // What to do about it.
  children?: ReactNode
  // The buttons.
  actions?: ReactNode
  // A visual above the title.
  icon?: ReactNode
}

export function EmptyState({ title, children, actions, icon }: EmptyStateProps) {
  return (
    <div className="ui-empty">
      {icon && (
        <span className="ui-empty-icon" aria-hidden="true">
          {icon}
        </span>
      )}
      <p className="ui-empty-title">{title}</p>
      {children && <div className="ui-empty-body">{children}</div>}
      {actions && <div className="ui-empty-actions">{actions}</div>}
    </div>
  )
}
