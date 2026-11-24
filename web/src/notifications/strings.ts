import type { T } from '../i18n'

/**
 * What the service worker says for each kind of push, in the person's language: the worker can't
 * load the app's translations, so the page hands it these when push is on and the language changes.
 * A push carries only the kind and the notification's ID, never a title or a name.
 */
export function pushStrings(t: T): Record<string, string> {
  return {
    card_assigned: t('notifications.push.card_assigned'),
    card_due: t('notifications.push.card_due'),
    card_overdue: t('notifications.push.card_overdue'),
    app_effect_failed: t('notifications.push.app_effect_failed'),
    changed: t('notifications.push.changed'),
  }
}
