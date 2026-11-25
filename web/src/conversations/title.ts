import type { Conversation } from '../api'
import { format, t as translate } from '../i18n'

// Its title, or for an untitled one, when it began. Outside a hook, for lists elsewhere too: a
// component that shows it also calls useT(), so it re-renders when the language changes.
export function conversationTitle(c: Pick<Conversation, 'title' | 'created_at'>): string {
  return c.title.trim() || translate('conversations.untitled', { date: format().dateTime(c.created_at) })
}
