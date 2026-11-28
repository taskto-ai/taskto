import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { expect, test } from 'vitest'

const css = readFileSync(resolve(__dirname, 'AppAvatar.css'), 'utf8')

test('an emoji wider than its avatar, as joined pictographs the system has no single glyph for are, is clipped to it', () => {
  expect(css).toMatch(/\.ui-avatar\.apps-avatar-icon\s*\{[^}]*overflow:\s*hidden/)
})
