// The menu's stylesheet, read as text: what a browser would lay out, which jsdom doesn't.

import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from 'vitest'

const css = readFileSync(join(__dirname, 'Menu.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '')

// The declarations of the one rule with exactly this selector.
function declarations(selector: string): Record<string, string> {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const match = css.match(new RegExp(`(?:^|[}\\s])${escaped}\\s*\\{([^}]*)\\}`))
  if (!match) throw new Error(`no rule for ${selector}`)
  return Object.fromEntries(
    match[1]
      .split(';')
      .map((d) => d.split(':').map((p) => p.trim()))
      .filter(([name, value]) => name && value)
      .map(([name, ...rest]) => [name, rest.join(':')]),
  )
}

test("a long unbroken name wraps inside the menu's box instead of widening its column", () => {
  // A grid's implicit column is as wide as its content: a column held to the list's width, and a
  // label that may break anywhere, keep the check marks in sight.
  expect(declarations('.ui-menu-list:popover-open')['grid-template-columns']).toBe('minmax(0, 1fr)')
  expect(declarations('.ui-menu-group')['grid-template-columns']).toBe('minmax(0, 1fr)')
  expect(declarations('.ui-menu-item-label')['overflow-wrap']).toBe('anywhere')
})

test("a radio item's check mark is in the text's colour, not the icons' muted one", () => {
  // `.ui-menu-item > svg` mutes an item's icon; the check mark's rule must be at least as specific
  // and come after it.
  const check = declarations('.ui-menu-item > .ui-menu-item-check')
  expect(check.color).toBe('var(--color-text)')
  expect(css.indexOf('.ui-menu-item > .ui-menu-item-check')).toBeGreaterThan(css.indexOf('.ui-menu-item > svg'))
})

test('the gap between grouped items is the thin line, a token', () => {
  expect(declarations('.ui-menu-group').gap).toBe('var(--line-thin)')
})
