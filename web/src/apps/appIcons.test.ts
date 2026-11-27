import { expect, test } from 'vitest'
import { APP_ICONS, isAppIcon } from './appIcons'

test('an icon is one emoji of the kinds the server accepts', () => {
  for (const icon of [...APP_ICONS, ' 🧾 ', '👍🏽', '👨‍👩‍👧‍👦', '🇧🇷', '❤️']) expect(isAppIcon(icon), icon).toBe(true)
})

test('letters, digits, punctuation, spaces and several emoji are refused', () => {
  for (const icon of ['', ' ', 'a', 'abc', '1', '1️⃣', '<', '-', '🧾a', '🧾 🧾', '🧾🧾', '👩🏽‍❤️‍💋‍👨🏾'])
    expect(isAppIcon(icon), icon).toBe(false)
})

test('the list offers about forty, each once', () => {
  expect(new Set(APP_ICONS).size).toBe(APP_ICONS.length)
  expect(APP_ICONS.length).toBeGreaterThanOrEqual(40)
})
