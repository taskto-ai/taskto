// The shell's stylesheet, read as text: the tablet's rail, which jsdom doesn't lay out.

import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from 'vitest'

const css = readFileSync(join(__dirname, 'Shell.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '')

// The text of the block that starts with `head`, its braces balanced.
function block(source: string, head: string): string {
  const at = source.indexOf(head)
  if (at < 0) throw new Error(`no ${head}`)
  const open = source.indexOf('{', at)
  let depth = 0
  for (let i = open; i < source.length; i++) {
    if (source[i] === '{') depth++
    if (source[i] === '}' && --depth === 0) return source.slice(open + 1, i)
  }
  throw new Error(`${head} is not closed`)
}

// The declarations of the rule with exactly this selector, in `source`.
function declarations(source: string, selector: string): Record<string, string> {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const match = source.match(new RegExp(`(?:^|[}\\s])${escaped}\\s*\\{([^}]*)\\}`))
  if (!match) throw new Error(`no rule for ${selector}`)
  return Object.fromEntries(
    match[1]
      .split(';')
      .map((d) => d.split(':').map((p) => p.trim()))
      .filter(([name, value]) => name && value)
      .map(([name, ...rest]) => [name, rest.join(':')]),
  )
}

const rail = block(css, '@media (min-width: 48.0001rem) and (max-width: 56.25rem)')

test("the rail's column holds a 44px control, the sidebar's padding and its edge, so nothing overflows", () => {
  expect(declarations(rail, '.ui-shell')['grid-template-columns']).toBe(
    'calc(var(--control-touch) + var(--space-4) + var(--line-thin)) minmax(0, 1fr)',
  )
  expect(declarations(rail, '.ui-shell-sidebar')['padding-inline']).toBe('var(--space-2)')
})

test("in the rail, the inbox's count sits out on the bell's corner, not on its top", () => {
  const count = declarations(rail, '.ui-shell-count')
  expect(count['inset-block-start']).toBe('calc(-1 * var(--space-1))')
  expect(count['inset-inline-end']).toBe('calc(-1 * var(--space-2))')
})

test("the rail's workspace switcher has the links' hover ground", () => {
  expect(declarations(rail, '.ui-shell-switcher-button.ui-button:hover').background).toBe('var(--color-hover)')
})

test('a box cut to the logo is as wide as the logo is high, at any root font size', () => {
  expect(declarations(css, '.ui-shell-bar-mark')['inline-size']).toBe('var(--logo-height)')
  expect(declarations(rail, '.ui-shell-logo')['inline-size']).toBe('var(--logo-height)')
  const tokens = readFileSync(join(__dirname, '..', 'ui', 'tokens.css'), 'utf8')
  const legacy = readFileSync(join(__dirname, '..', 'legacy-shared.css'), 'utf8')
  const height = tokens.match(/--logo-height:\s*([^;]+);/)?.[1]
  expect(height).toBe('1.5rem')
  expect(declarations(legacy, '.tt-logo').height).toBe(height)
})
