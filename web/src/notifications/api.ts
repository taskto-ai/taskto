import { ws } from '../api'
import type { Section } from '../router'

export type NotificationKind =
  | 'card_assigned'
  | 'card_comment'
  | 'card_mention'
  | 'card_due'
  | 'card_overdue'
  | 'app_effect_failed'
  | 'app_schedule_failed'
  | 'integration_event'
  | 'changed'

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

export const notificationPath = (id: number) => `/notification/${id}`

export function parseNotificationPath(path: string): number | null {
  const m = /^\/notification\/(\d+)\/?$/.exec(path)
  return m ? Number(m[1]) : null
}

export const INBOX_CHANGED = 'taskto:inbox-changed'

export type EmailMode = 'off' | 'instant' | 'digest'
export type EmailKind =
  | 'card_assigned'
  | 'card_comment'
  | 'card_mention'
  | 'card_due'
  | 'card_overdue'
  | 'app_effect_failed'
  | 'app_schedule_failed'
export const EMAIL_KINDS: EmailKind[] = [
  'card_assigned',
  'card_comment',
  'card_mention',
  'card_due',
  'card_overdue',
  'app_effect_failed',
  'app_schedule_failed',
]
export type EmailAddress = { email: string; state: 'unverified' | 'pending' | 'verified' }
export type EmailChoices = {
  available: boolean
  address: EmailAddress
  kinds: Record<EmailKind, EmailMode>
  digest: { hour: number; timezone: string; chosen: boolean }
}
export const emailPath = (workspace: number) => `${ws(workspace)}/notification-email`
export const emailVerificationPath = (workspace: number) => `${emailPath(workspace)}/verification`
export const EMAIL_CONFIRM_API = '/api/me/notification-email/confirm'

export const EMAIL_CONFIRM_PATH = '/confirm-notification-email'
