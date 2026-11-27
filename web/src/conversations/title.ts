import type { Conversation } from '../api'
import { format, t as translate } from '../i18n'

export function conversationTitle(c: Pick<Conversation, 'title' | 'created_at'>): string {
  return c.title.trim() || translate('conversations.untitled', { date: format().dateTime(c.created_at) })
}
