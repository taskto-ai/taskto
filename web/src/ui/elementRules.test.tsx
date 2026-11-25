// @vitest-environment jsdom

// The old global stylesheet (src/styles.css) is in no cascade layer, so a rule of it that matched
// an element a component draws would beat the component's own style. This draws every component
// (each gallery, and the overlays open) and checks that no rule of that stylesheet reaches any
// element with a ui- class, except the ones css-check allows on purpose. css-check guards the
// spelling of the rules; this guards what they match.

import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { act, cleanup, fireEvent, render } from '@testing-library/react'
import type { ComponentType } from 'react'
import { afterEach, expect, test } from 'vitest'
// @ts-expect-error: a plain ES module script, without types
import { allRules, ELEMENT_RULES, isElementRule } from '../../scripts/css-check.mjs'
import { Dialog } from './Dialog'
import { Drawer } from './Drawer'
import { MenuItem, MoreMenu } from './Menu'
import { PageHeader } from './PageHeader'
import { resetToasts, toast } from './Toast'

afterEach(() => {
  act(() => resetToasts())
  cleanup()
})

const css = readFileSync(join(__dirname, '../styles.css'), 'utf8')
const rules: { selector: string }[] = allRules(css).filter(
  (r: { selector: string }) =>
    isElementRule(r.selector) && !ELEMENT_RULES.has(r.selector) && !r.selector.includes('::'),
)

const galleries = import.meta.glob<{ default: ComponentType; title: string }>('./*.gallery.tsx', { eager: true })

function drawEverything() {
  render(
    <>
      {Object.values(galleries).map(({ title, default: Specimens }) => (
        <section key={title}>
          <Specimens />
        </section>
      ))}
      <PageHeader title="t" help="h" />
      <Dialog open onClose={() => {}} title="t" description="d" actions={<button type="button">a</button>}>
        <p className="ui-dialog-p">b</p>
      </Dialog>
      <Drawer open onClose={() => {}} title="t" footer={<span />}>
        <span />
      </Drawer>
      <MoreMenu>
        <MenuItem onSelect={() => {}}>m</MenuItem>
      </MoreMenu>
    </>,
  )
  fireEvent.click(document.querySelectorAll('[aria-haspopup="menu"]')[0])
  act(() => {
    toast({ message: 'x', action: { label: 'y', onClick: () => {} } })
  })
}

test('the old stylesheet has element rules to check', () => {
  expect(rules.length).toBeGreaterThan(15)
  // They still reach the screens' own elements, which have no ui- class.
  document.body.innerHTML = '<h2 class="tt-x">a</h2><p>b</p><a href="#">c</a><button>d</button><code>e</code>'
  for (const el of document.body.children) {
    expect(
      rules.some((r) => el.matches(r.selector)),
      el.outerHTML,
    ).toBe(true)
  }
})

test('no rule of the old stylesheet reaches an element a component draws', () => {
  drawEverything()
  const drawn = [...document.querySelectorAll<HTMLElement>('[class^="ui-"], [class*=" ui-"]')]
  expect(drawn.length).toBeGreaterThan(200)
  const kinds = new Set(drawn.map((el) => el.tagName.toLowerCase()))
  for (const tag of ['h1', 'h2', 'p', 'a', 'button', 'input', 'label', 'table', 'th', 'td', 'li', 'ul', 'dialog']) {
    expect(kinds, tag).toContain(tag)
  }
  const reached: string[] = []
  for (const el of drawn) {
    for (const { selector } of rules) {
      if (el.matches(selector)) reached.push(`${selector} → <${el.tagName.toLowerCase()} class="${el.className}">`)
    }
  }
  expect(reached).toEqual([])
})
