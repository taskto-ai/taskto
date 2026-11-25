// A message about this place: something is off, a key was refused, a change is waiting. An icon
// and words on a full tint of the tone (never a bar down one side).
//
// The role follows the tone: danger is an alert (said at once when it appears), info, success
// and warning a status (said when it changes), neutral nothing (it is part of the page). A notice
// that is there when the page opens is read with the page whatever its role.
//
// A notice that is part of what the page shows, not news (an error a screen draws after its data
// loads, every time it is visited), is `live={false}`: no role at all, so it is read with the page
// and not announced on every visit.

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
  // A short line in bold before the words.
  title?: ReactNode
  children: ReactNode
  // A button or a link at the end: "Set up email", "Try again".
  action?: ReactNode
  // False: part of the page, never announced (see above). The tone still shows in its colour and
  // its icon.
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
