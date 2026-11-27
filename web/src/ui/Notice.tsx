import type { ReactNode } from 'react'
import { Icon } from './Icon'
import './Notice.css'

export type NoticeTone = 'info' | 'success' | 'warning' | 'danger' | 'neutral'

const ICONS = { info: 'info', success: 'check', warning: 'alert', danger: 'alert', neutral: 'info' } as const

const ROLES: Record<NoticeTone, 'alert' | 'status' | undefined> = {
  danger: 'alert',
  warning: 'status',
  success: 'status',
  info: 'status',
  neutral: undefined,
}

export interface NoticeProps {
  tone?: NoticeTone
  title?: ReactNode
  children: ReactNode
  action?: ReactNode
  live?: boolean
}

export function Notice({ tone = 'info', title, children, action, live = true }: NoticeProps) {
  return (
    <div className="ui-notice" data-tone={tone} role={live ? ROLES[tone] : undefined}>
      <span className="ui-notice-icon" aria-hidden="true">
        <Icon name={ICONS[tone]} />
      </span>
      <div className="ui-notice-body">
        {title && <p className="ui-notice-title">{title}</p>}
        <div className="ui-notice-text">{children}</div>
      </div>
      {action && <div className="ui-notice-action">{action}</div>}
    </div>
  )
}
