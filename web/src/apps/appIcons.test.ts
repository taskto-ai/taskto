import { expect, test, vi } from 'vitest'
import vectors from '../../../internal/apps/testdata/app_icons.json'
import { APP_ICONS, isAppIcon } from './appIcons'
import { PICTOGRAPH_RANGES } from './pictographs'

test('an icon is one emoji of the kinds the server accepts', () => {
  for (const icon of [...APP_ICONS, ...vectors.valid]) expect(isAppIcon(icon), icon).toBe(true)
})

test('letters, digits, punctuation, spaces, invisible characters and several emoji are refused, as by the server', () => {
  expect(isAppIcon('')).toBe(false)
  for (const icon of vectors.invalid) expect(isAppIcon(icon), JSON.stringify(icon)).toBe(false)
})

test('an emoji is not tested for one grapheme with a browser feature that may be missing at load', async () => {
  const segmenter = Intl.Segmenter
  Object.defineProperty(Intl, 'Segmenter', { value: undefined, configurable: true })
  try {
    vi.resetModules()
    const fresh = await import('./appIcons')
    expect(fresh.isAppIcon('🧾🧾')).toBe(false)
    expect(fresh.isAppIcon('🧾')).toBe(true)
    expect(fresh.isAppIcon('👨\u200d👩\u200d👧\u200d👦')).toBe(true)
  } finally {
    Object.defineProperty(Intl, 'Segmenter', { value: segmenter, configurable: true })
    vi.resetModules()
  }
})

test('the pictographs the picker knows are the shared table, whatever Unicode version this runtime has', () => {
  expect(PICTOGRAPH_RANGES).toEqual(vectors.pictographic.flat())
  const listed = (cp: number) => vectors.pictographic.some(([from, to]) => cp >= from && cp <= to)
  for (const [from, to] of vectors.pictographic) {
    for (const cp of [from - 1, from, to, to + 1]) {
      if (cp < 1 || (cp >= 0xd800 && cp <= 0xdfff)) continue
      const alone = isAppIcon(String.fromCodePoint(cp))
      expect(alone, cp.toString(16)).toBe(listed(cp))
    }
  }
})

test('the list offers about forty, each once', () => {
  expect(new Set(APP_ICONS).size).toBe(APP_ICONS.length)
  expect(APP_ICONS.length).toBeGreaterThanOrEqual(40)
})
