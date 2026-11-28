import { PICTOGRAPH_RANGES } from './pictographs'

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

const codePoint = (n: number) => `\\u{${n.toString(16)}}`
const pictograph = `[${PICTOGRAPH_RANGES.map((n, i) => (i % 2 === 0 ? codePoint(n) : `-${codePoint(n)}`)).join('')}]`
const joinable = `${pictograph}(?:\\u{FE0F}|[\\u{1F3FB}-\\u{1F3FF}])?`
const shape = [
  '[0-9#*]\\u{FE0F}?\\u{20E3}',
  '[\\u{1F1E6}-\\u{1F1FF}]{2}',
  '\\u{1F3F4}[\\u{E0020}-\\u{E007E}]+\\u{E007F}',
  `${joinable}(?:\\u{200D}${joinable})*`,
]
const emoji = new RegExp(`^(?:${shape.join('|')})$`, 'u')

export function isAppIcon(text: string): boolean {
  if (text === '') return false
  if (new TextEncoder().encode(text).length > MAX_BYTES || Array.from(text).length > MAX_CHARACTERS) return false
  return emoji.test(text)
}
