// @vitest-environment jsdom

import { readFileSync } from 'node:fs'
import { beforeEach, expect, test } from 'vitest'
import { applyTheme, THEME_COLOR } from './theme'

const read = (path: string) => readFileSync(new URL(path, import.meta.url), 'utf8')

function metas() {
  document.head.innerHTML = `
    <meta name="theme-color" content="${THEME_COLOR.light}" media="(prefers-color-scheme: light)" />
    <meta name="theme-color" content="${THEME_COLOR.dark}" media="(prefers-color-scheme: dark)" />`
  return [...document.querySelectorAll<HTMLMetaElement>('meta[name="theme-color"]')]
}

beforeEach(() => {
  delete document.documentElement.dataset.theme
})

test("the browser's chrome follows the picked theme, whatever the system's", () => {
  const [light, dark] = metas()

  applyTheme('dark')
  expect([light.content, dark.content]).toEqual([THEME_COLOR.dark, THEME_COLOR.dark])
  applyTheme('light')
  expect([light.content, dark.content]).toEqual([THEME_COLOR.light, THEME_COLOR.light])
})

test("the system's theme puts back each meta's own colour, and keeps its media query", () => {
  const [light, dark] = metas()

  applyTheme('dark')
  applyTheme('system')
  expect([light.content, dark.content]).toEqual([THEME_COLOR.light, THEME_COLOR.dark])
  expect(light.getAttribute('media')).toBe('(prefers-color-scheme: light)')
  expect(dark.getAttribute('media')).toBe('(prefers-color-scheme: dark)')
  applyTheme('light')
  applyTheme('system')
  expect([light.content, dark.content]).toEqual([THEME_COLOR.light, THEME_COLOR.dark])
})

test('a page without theme-color metas still takes a theme', () => {
  document.head.innerHTML = ''
  applyTheme('dark')
  expect(document.documentElement.dataset.theme).toBe('dark')
})

test("the colours are the page's: the ground role, index.html's metas and the manifest", () => {
  expect(read('./ui/themes/mint.css')).toContain(`--color-bg: light-dark(${THEME_COLOR.light}, ${THEME_COLOR.dark});`)
  expect(read('./ui/legacy.css')).toContain('--tt-canvas: var(--color-bg);')

  const html = read('../index.html')
  expect(html).toContain(`name="theme-color" content="${THEME_COLOR.light}" media="(prefers-color-scheme: light)"`)
  expect(html).toContain(`name="theme-color" content="${THEME_COLOR.dark}" media="(prefers-color-scheme: dark)"`)

  const manifest = JSON.parse(read('../public/manifest.webmanifest')) as { theme_color: string }
  expect(manifest.theme_color).toBe(THEME_COLOR.light)
})
