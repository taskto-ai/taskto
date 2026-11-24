// The notification inbox's API (docs/api.md#notifications) and where its items lead.

import { ws } from '../api'
import type { Section } from '../router'

export type NotificationKind = 'card_assigned' | 'card_due' | 'card_overdue' | 'app_effect_failed' | 'changed'

// An item as the person may read it now: `changed` says nothing of what it was about.
export type NotificationItem = {
  id: number
  workspace_id: number
  kind: NotificationKind
  params?: Record<string, string>
  target: { section: Section; item?: string }
  read: boolean
  created_at: string
}

export type NotificationList = { notifications: NotificationItem[]; more: boolean; unread: number }

export const inboxPath = (workspace: number) => `${ws(workspace)}/notifications`

/** The address a push opens: it holds the notification's own ID and nothing else. */
export const notificationPath = (id: number) => `/notification/${id}`

export function parseNotificationPath(path: string): number | null {
  const m = /^\/notification\/(\d+)\/?$/.exec(path)
  return m ? Number(m[1]) : null
}

// Raised after the inbox changed in this page (something was read), so the badge asks again.
export const INBOX_CHANGED = 'taskto:inbox-changed'

// The email choices of the caller in a workspace (docs/api.md#notification-email): per kind, off,
// instant (a mail soon after) or digest (counted in a daily summary). `available` is false when the
// server can't send mail, and then nothing can be turned on.
export type EmailMode = 'off' | 'instant' | 'digest'
export type EmailKind = 'card_assigned' | 'app_effect_failed'
export const EMAIL_KINDS: EmailKind[] = ['card_assigned', 'app_effect_failed']
export type EmailChoices = {
  available: boolean
  kinds: Record<EmailKind, EmailMode>
  digest: { hour: number; timezone: string }
}
export const emailPath = (workspace: number) => `${ws(workspace)}/notification-email`
