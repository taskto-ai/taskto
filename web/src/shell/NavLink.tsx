import type { ReactNode } from 'react'
import { Link } from '../router'
import { Tooltip } from '../ui/Tooltip'

export interface NavLinkProps {
  href: string
  current: boolean
  // The icon or the avatar before the label.
  leading: ReactNode
  label: string
  // After the label: a count.
  trailing?: ReactNode
  // The link's name when it says more than its label (the inbox's unread count).
  name?: string
  // The label is someone's words (a pinned app's), not the app's own: it keeps its own direction.
  userText?: boolean
}

// One entry of the sidebar. The label is its name everywhere: in the rail it is hidden only from
// sight, never from assistive technology (Shell.css), and its tooltip, which says the same and is
// shown only in the rail, beside it, is hidden from assistive technology so it isn't said twice.
export function NavLink({ href, current, leading, label, trailing, name, userText = false }: NavLinkProps) {
  return (
    <Tooltip content={label} describes={false} side="inline-end">
      <Link
        href={href}
        className="ui-control ui-shell-link"
        aria-current={current ? 'page' : undefined}
        aria-label={name}
      >
        {leading}
        <span className="ui-shell-link-label" dir={userText ? 'auto' : undefined}>
          {label}
        </span>
        {trailing}
      </Link>
    </Tooltip>
  )
}
