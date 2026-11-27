import type { Author, Card } from '../api'
import type { T } from '../i18n'

export function blockedText(t: T, reason: string | null, cards: string): string {
  if (reason && cards) return t('boards.blockedBecauseWaiting', { reason, cards })
  if (reason) return t('boards.blockedBecause', { reason })
  if (cards) return t('boards.blockedWaiting', { cards })
  return t('boards.blocked')
}

export const assigneeLabel = (t: T, a: NonNullable<Card['assignee']>) =>
  a.kind === 'agent'
    ? t('boards.agent')
    : a.kind === 'deleted'
      ? t('common.deletedUser')
      : (a.name ?? t('boards.assigned'))

export const authorLabel = (t: T, a: Author) =>
  !a
    ? t('boards.someone')
    : !a.agent
      ? a.name
      : a.via
        ? t('boards.byAgentVia', { name: a.name, via: a.via })
        : t('boards.byAgent', { name: a.name })
