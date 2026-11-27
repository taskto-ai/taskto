import type { ReactNode } from 'react'
import { Link } from '../router'
import { Tooltip } from '../ui/Tooltip'

export interface NavLinkProps {
  href: string
  current: boolean
  leading: ReactNode
  label: string
  trailing?: ReactNode
  name?: string
  userText?: boolean
}

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
