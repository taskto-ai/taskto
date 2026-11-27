import type { T } from '../i18n'

export function pushStrings(t: T): Record<string, string> {
  return {
    card_assigned: t('notifications.push.card_assigned'),
    card_due: t('notifications.push.card_due'),
    card_overdue: t('notifications.push.card_overdue'),
    card_comment: t('notifications.push.card_comment'),
    card_mention: t('notifications.push.card_mention'),
    app_effect_failed: t('notifications.push.app_effect_failed'),
    integration_event: t('notifications.push.integration_event'),
    changed: t('notifications.push.changed'),
  }
}
