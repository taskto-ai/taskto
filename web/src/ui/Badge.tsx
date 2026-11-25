// A short status in a pill: Live, Draft, Overdue, Offline. The tone colours it, and its words say
// it, so the colour is never the only sign. A dot or an icon may go before the words.

import type { ReactNode } from 'react'
import { Icon, type IconName } from './Icon'
import './Badge.css'

export type BadgeTone = 'success' | 'warning' | 'danger' | 'info' | 'neutral'

const TONE_CLASS = {
  success: 'ui-badge-success',
  warning: 'ui-badge-warning',
  danger: 'ui-badge-danger',
  info: 'ui-badge-info',
  neutral: 'ui-badge-neutral',
} as const

export interface BadgeProps {
  tone?: BadgeTone
  dot?: boolean
  icon?: IconName
  className?: string
  children: ReactNode
}

export function Badge({ tone = 'neutral', dot = false, icon, className, children }: BadgeProps) {
  return (
    <span className={['ui-badge', TONE_CLASS[tone], className ?? ''].filter(Boolean).join(' ')}>
      {dot && <span className="ui-badge-dot" aria-hidden="true" />}
      {icon && <Icon name={icon} size={12} />}
      {children}
    </span>
  )
}
