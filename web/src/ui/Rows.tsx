import type { ReactNode } from 'react'
import { Link } from '../router'
import './Rows.css'

export interface RowsProps {
  label?: string
  ordered?: boolean
  children: ReactNode
}

export function Rows({ label, ordered = false, children }: RowsProps) {
  const List = ordered ? 'ol' : 'ul'
  return (
    <List className="ui-rows" role="list" aria-label={label}>
      {children}
    </List>
  )
}

export interface RowProps {
  title: ReactNode
  detail?: ReactNode
  leading?: ReactNode
  status?: ReactNode
  actions?: ReactNode
  actionsBelow?: boolean
  href?: string
  onOpen?: () => void
  current?: boolean
  children?: ReactNode
}

export function Row({
  title,
  detail,
  leading,
  status,
  actions,
  actionsBelow,
  href,
  onOpen,
  current,
  children,
}: RowProps) {
  return (
    <li
      className="ui-row"
      data-link={href || onOpen ? true : undefined}
      data-current={current || undefined}
      data-actions-below={actionsBelow || undefined}
    >
      {leading && <span className="ui-row-leading">{leading}</span>}
      <span className="ui-row-main">
        {href ? (
          <Link className="ui-row-title" href={href} onClick={onOpen} aria-current={current ? 'page' : undefined}>
            {title}
          </Link>
        ) : onOpen ? (
          <button
            type="button"
            className="ui-control ui-row-title ui-row-open"
            onClick={onOpen}
            aria-current={current || undefined}
          >
            {title}
          </button>
        ) : (
          <span className="ui-row-title">{title}</span>
        )}
        {detail && <span className="ui-row-detail">{detail}</span>}
      </span>
      {status && <span className="ui-row-status">{status}</span>}
      {actions && <span className="ui-row-actions">{actions}</span>}
      {children && <div className="ui-row-more">{children}</div>}
    </li>
  )
}
