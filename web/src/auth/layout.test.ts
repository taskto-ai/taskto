import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from 'vitest'

// jsdom draws no layout: these read the sign-in frame's rules themselves.
const css = readFileSync(join(__dirname, 'AuthLayout.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '')

test('on a touch screen the Show button keeps the 44px target; only what it draws keeps inside the field', () => {
  const coarse = css.slice(css.indexOf('@media (pointer: coarse)'))
  const block = coarse.slice(0, coarse.indexOf('\n  }\n') + 4)
  expect(block).toContain('.ui-auth-password > .ui-button')
  // No height of its own: the components' touch height (44px) stands.
  expect(block).not.toMatch(/block-size|height/)
  expect(block).toMatch(/background-clip:\s*content-box/)
  expect(block).toMatch(/outline-offset:\s*calc\(var\(--space-1\) \* -1\)/)
})
