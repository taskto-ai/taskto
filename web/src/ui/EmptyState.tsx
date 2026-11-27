import type { ReactNode } from 'react'
import './EmptyState.css'

export interface EmptyStateProps {
  title: ReactNode
  children?: ReactNode
  actions?: ReactNode
  icon?: ReactNode
  level?: 1 | 2 | 3 | 4 | 5 | 6
}

export function EmptyState({ title, children, actions, icon, level }: EmptyStateProps) {
  const Title = level ? (`h${level}` as const) : 'p'
  return (
    <div className="ui-empty">
      {icon && (
        <span className="ui-empty-icon" aria-hidden="true">
          {icon}
        </span>
      )}
      <Title className="ui-empty-title">{title}</Title>
      {children && <div className="ui-empty-body">{children}</div>}
      {actions && <div className="ui-empty-actions">{actions}</div>}
    </div>
  )
}
