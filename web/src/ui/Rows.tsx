// A list of objects (apps, invites, documents): each row a leading visual, a title and a line of
// detail, a status, and the row's actions at the end. A row with `href` is a link: the title is
// the link, and the whole row answers a click on it, while the actions stay their own buttons.

import type { ReactNode } from 'react'
import { Link } from '../router'
import './Rows.css'

export interface RowsProps {
  // What the list is, for a screen reader, when no heading says it.
  label?: string
  children: ReactNode
}

export function Rows({ label, children }: RowsProps) {
  return (
    <ul className="ui-rows" aria-label={label}>
      {children}
    </ul>
  )
}

export interface RowProps {
  title: ReactNode
  detail?: ReactNode
  // An avatar, an icon, an app's mark.
  leading?: ReactNode
  // A badge: live, a draft, pending.
  status?: ReactNode
  // Buttons and a menu, at the end.
  actions?: ReactNode
  // The row opens this, in the app.
  href?: string
  // Called when the row's link is followed, before the app goes there (to mark it read).
  onOpen?: () => void
  current?: boolean
}

export function Row({ title, detail, leading, status, actions, href, onOpen, current }: RowProps) {
  return (
    <li className="ui-row" data-link={href ? true : undefined} data-current={current || undefined}>
      {leading && <span className="ui-row-leading">{leading}</span>}
      <span className="ui-row-main">
        {href ? (
          <Link className="ui-row-title" href={href} onClick={onOpen} aria-current={current ? 'page' : undefined}>
            {title}
          </Link>
        ) : (
          <span className="ui-row-title">{title}</span>
        )}
        {detail && <span className="ui-row-detail">{detail}</span>}
      </span>
      {status && <span className="ui-row-status">{status}</span>}
      {actions && <span className="ui-row-actions">{actions}</span>}
    </li>
  )
}
