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
}

// One entry of the sidebar. Its tooltip is its label, for the rail, where the label is hidden (the
// tooltip is drawn only there, Shell.css); the label stays in the link, so it is always its name.
export function NavLink({ href, current, leading, label, trailing, name }: NavLinkProps) {
  return (
    <Tooltip content={label} describes={false}>
      <Link
        href={href}
        className="ui-control ui-shell-link"
        aria-current={current ? 'page' : undefined}
        aria-label={name}
      >
        {leading}
        <span className="ui-shell-link-label">{label}</span>
        {trailing}
      </Link>
    </Tooltip>
  )
}
