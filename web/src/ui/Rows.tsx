// A list of objects (apps, invites, documents): each row a leading visual, a title and a line of
// detail, a status, and the row's actions at the end. A row with `href` is a link: the title is
// the link, and the whole row answers a click on it, while the actions stay their own buttons.
// A row with `onOpen` and no `href` opens something in place (a search result, a version): the
// title is a button, and the whole row answers a click on it the same way.

import type { ReactNode } from 'react'
import { Link } from '../router'
import './Rows.css'

export interface RowsProps {
  // What the list is, for a screen reader, when no heading says it.
  label?: string
  // Rows whose order means something (the steps of a checklist): an ordered list.
  ordered?: boolean
  children: ReactNode
}

export function Rows({ label, ordered = false, children }: RowsProps) {
  const List = ordered ? 'ol' : 'ul'
  return (
    <List className="ui-rows" aria-label={label}>
      {children}
    </List>
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
  // The actions on a line of their own under the title, wrapping there when they don't fit: for a
  // row with several actions (more than a phone's width holds beside the title).
  actionsBelow?: boolean
  // The row opens this, in the app.
  href?: string
  // Called when the row's link is followed, before the app goes there; with no `href`, what
  // the row opens in place, its title then being a button.
  onOpen?: () => void
  current?: boolean
  // What opens under the row, across its width (a form to settle it, its details): part of the row,
  // in its list item, so what it says belongs to the row.
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
