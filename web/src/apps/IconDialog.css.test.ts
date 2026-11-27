import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { expect, test } from 'vitest'

const css = readFileSync(resolve(__dirname, 'IconDialog.css'), 'utf8')

test('on a touch screen the icon tiles are as wide and as tall as a touch target, so fewer fit in a row', () => {
  const coarse = css.match(/@media \(pointer: coarse\)\s*\{([\s\S]*?)\n {2}\}/)
  expect(coarse).not.toBeNull()
  const block = coarse?.[1] ?? ''
  expect(block).toMatch(/\.apps-icon-grid\s*\{[^}]*minmax\(var\(--control-touch\),\s*1fr\)/)
  expect(block).toMatch(/\.apps-icon-face\s*\{[^}]*min-block-size:\s*var\(--control-touch\)/)
})
