export const APP_ICONS = [
  '🧾',
  '💰',
  '📊',
  '📈',
  '📋',
  '📝',
  '📅',
  '⏰',
  '✅',
  '📦',
  '🛒',
  '🏷️',
  '🔧',
  '🛠️',
  '🚚',
  '🚗',
  '✈️',
  '🏠',
  '🏢',
  '👥',
  '💬',
  '📧',
  '📞',
  '📷',
  '🎯',
  '💡',
  '⭐',
  '❤️',
  '🔥',
  '🌱',
  '🍽️',
  '☕',
  '🎓',
  '🩺',
  '🧪',
  '📚',
  '🔒',
  '🎁',
  '🎉',
  '🗂️',
] as const

const MAX_BYTES = 32
const MAX_CHARACTERS = 8

const segmenter =
  typeof Intl !== 'undefined' && 'Segmenter' in Intl ? new Intl.Segmenter(undefined, { granularity: 'grapheme' }) : null

export function isAppIcon(text: string): boolean {
  const icon = text.trim()
  if (icon === '') return false
  if (new TextEncoder().encode(icon).length > MAX_BYTES || Array.from(icon).length > MAX_CHARACTERS) return false
  if (/[\p{L}\p{Nd}\p{P}\p{Z}\p{Cc}\s]/u.test(icon)) return false
  if (!/\p{Extended_Pictographic}|\p{Regional_Indicator}/u.test(icon)) return false
  return segmenter ? Array.from(segmenter.segment(icon)).length === 1 : true
}
