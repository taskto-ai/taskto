import { expect, test } from 'vitest'
import vectors from '../../../internal/apps/testdata/app_icons.json'
import { APP_ICONS, isAppIcon } from './appIcons'

test('an icon is one emoji of the kinds the server accepts', () => {
  for (const icon of [...APP_ICONS, ...vectors.valid]) expect(isAppIcon(icon), icon).toBe(true)
})

test('letters, digits, punctuation, spaces, invisible characters and several emoji are refused, as by the server', () => {
  expect(isAppIcon('')).toBe(false)
  for (const icon of vectors.invalid) expect(isAppIcon(icon), JSON.stringify(icon)).toBe(false)
})

test('an emoji is not tested for one grapheme with a browser feature that may be missing', () => {
  const segmenter = Intl.Segmenter
  Object.defineProperty(Intl, 'Segmenter', { value: undefined, configurable: true })
  try {
    expect(isAppIcon('🧾🧾')).toBe(false)
    expect(isAppIcon('🧾')).toBe(true)
  } finally {
    Object.defineProperty(Intl, 'Segmenter', { value: segmenter, configurable: true })
  }
})

test('the pictographs the server knows are those of this browser, code point by code point', () => {
  const pictograph = /^\p{Extended_Pictographic}$/u
  const listed = (cp: number) => vectors.pictographic.some(([from, to]) => cp >= from && cp <= to)
  const differ: string[] = []
  for (let cp = 1; cp <= 0x10ffff; cp++) {
    if (cp >= 0xd800 && cp <= 0xdfff) continue
    if (pictograph.test(String.fromCodePoint(cp)) !== listed(cp)) differ.push(cp.toString(16))
  }
  expect(differ).toEqual([])
})

test('the list offers about forty, each once', () => {
  expect(new Set(APP_ICONS).size).toBe(APP_ICONS.length)
  expect(APP_ICONS.length).toBeGreaterThanOrEqual(40)
})
