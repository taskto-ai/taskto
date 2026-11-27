// @vitest-environment jsdom

import { join } from 'node:path'
import { act, cleanup, fireEvent, render } from '@testing-library/react'
import type { ComponentType } from 'react'
import { afterEach, expect, test } from 'vitest'
import {
  allowedEverywhere,
  allRules,
  compounds,
  EVERYWHERE,
  OLD_STYLESHEETS,
  oldStylesheets,
  optsOut,
  reachesAnywhere,
} from '../../scripts/css-check.mjs'
import { ConfirmDialog } from './ConfirmDialog'
import { Dialog } from './Dialog'
import { Drawer } from './Drawer'
import { EmptyState } from './EmptyState'
import { Heading } from './Heading'
import { Icon } from './Icon'
import { Menu, MenuButton, MenuItem, MenuList, MenuSeparator } from './Menu'
import { Notice } from './Notice'
import { Page } from './Page'
import { PageHeader } from './PageHeader'
import { Panel } from './Panel'
import { Row, Rows } from './Rows'
import { Table } from './Table'
import { Tabs } from './Tabs'
import { resetToasts, Toaster, toast } from './Toast'
import { Toolbar, ToolbarSpacer } from './Toolbar'

afterEach(() => {
  act(() => resetToasts())
  cleanup()
  document.body.innerHTML = ''
})

interface Rule {
  selector: string
  properties: string[]
  line: number
}

const sheets = oldStylesheets(join(__dirname, '../..'))
const css = sheets.map((s) => s.css).join('\n')
const guarded: Rule[] = allRules(css).filter(
  (r: Rule) =>
    reachesAnywhere(r.selector) && !(EVERYWHERE.has(r.selector) && allowedEverywhere({ ...r, selector: r.selector })),
)

const STATES =
  /:(focus-visible|focus-within|focus|hover|active|checked|disabled|enabled|invalid|placeholder-shown|popover-open|open|modal)\b/g
function matchable(selector: string): string {
  const s = selector.replace(/::[\w-]+(\([^)]*\))?/g, '').replace(STATES, '')
  return s.trim() === '' || /^[>+~]/.test(s.trim()) ? '*' : s
}

function reaches(el: Element, rule: Rule): boolean {
  try {
    return el.matches(matchable(rule.selector))
  } catch {
    throw new Error(`jsdom can't test line ${rule.line}: ${rule.selector}`)
  }
}

const hasUi = (el: Element) => /(^|\s)ui-/.test(el.getAttribute('class') ?? '')

function Everything() {
  return (
    <>
      <Page width="reading">
        <PageHeader title="t" help="h" path="p" actions="a" />
        <Heading level={2}>s</Heading>
        <Toolbar label="f">
          x<ToolbarSpacer />
        </Toolbar>
        <Panel padded>
          <Rows label="r">
            <Row
              title="a"
              detail="d"
              leading={<Icon name="apps" />}
              status="s"
              actions="x"
              href="/w/1/apps/1"
              current
            />
            <Row title="b" />
          </Rows>
        </Panel>
        <Table
          caption="c"
          rows={[{ id: 1 }]}
          rowKey={(r) => r.id}
          columns={[
            { key: 'a', header: 'a', cell: () => 'x' },
            { key: 'n', header: 'n', numeric: true, hiddenHeader: true, cell: () => 1 },
          ]}
        />
        <Tabs
          id="t"
          label="t"
          value="a"
          items={[
            { id: 'a', label: 'a', count: 1 },
            { id: 'b', label: 'b' },
          ]}
        />
        <Tabs label="l" value="a" items={[{ id: 'a', label: 'a', href: '/a' }]} />
        <EmptyState title="e" icon={<Icon name="search" />} actions="x">
          w
        </EmptyState>
        <Notice tone="danger" title="n" action="x">
          w
        </Notice>
        <Menu>
          <MenuButton label="m" />
          <MenuList>
            <MenuItem icon="edit" onSelect={() => {}}>
              i
            </MenuItem>
            <MenuSeparator />
            <MenuItem tone="danger" disabled onSelect={() => {}}>
              d
            </MenuItem>
          </MenuList>
        </Menu>
        <Menu>
          <MenuButton label="s">v</MenuButton>
          <MenuList>
            <MenuItem onSelect={() => {}}>o</MenuItem>
          </MenuList>
        </Menu>
      </Page>
      <Dialog open onClose={() => {}} title="t" description="d" actions="a">
        b
      </Dialog>
      <ConfirmDialog open busy title="c" confirmLabel="d" onConfirm={() => {}} onCancel={() => {}}>
        w
      </ConfirmDialog>
      <Drawer open onClose={() => {}} title="t" headerActions="h" footer="f">
        b
      </Drawer>
      <Toaster />
    </>
  )
}

function drawEverything() {
  const { container } = render(<Everything />)
  for (const button of document.querySelectorAll('[aria-haspopup="menu"]')) fireEvent.click(button)
  act(() => {
    toast({ message: 'x', action: { label: 'y', onClick: () => {} } })
    toast({ message: 'z', tone: 'danger' })
  })
  return container
}

test("it reads the old stylesheets themselves: the base and every screen's file", () => {
  expect(sheets.map((s) => s.file)).toEqual(OLD_STYLESHEETS.map((f) => `src/${f}`))
})

test('every element a component draws has a ui- class', () => {
  const container = drawEverything()
  const drawn = [...document.body.querySelectorAll('*')].filter(
    (el) => el !== container && !el.closest('svg:not(:scope)') && el.tagName.toLowerCase() !== 'path',
  )
  expect(drawn.length).toBeGreaterThan(100)
  const bare = drawn.filter((el) => !hasUi(el) && !(el.tagName === 'svg' && el.closest('[class*="ui-"]')))
  expect(bare.map((el) => el.outerHTML.slice(0, 80))).toEqual([])
})

test('the old stylesheet has rules to check, and they still reach the screens’ own elements', () => {
  expect(guarded.length).toBeGreaterThan(20)
  expect(guarded.every((r) => optsOut(compounds(r.selector).at(-1) ?? ''))).toBe(true)
  document.body.innerHTML =
    '<h2 class="tt-x">a</h2><p>b</p><a href="#">c</a><button>d</button><code>e</code><input><select></select><textarea></textarea><fieldset></fieldset>'
  for (const el of document.body.children) {
    expect(
      guarded.some((r) => reaches(el, r)),
      el.outerHTML,
    ).toBe(true)
  }
  const button = document.querySelector('button') as Element
  expect(guarded.some((r) => r.selector.startsWith(':focus-visible') && reaches(button, r))).toBe(true)
  const input = document.querySelector('input') as Element
  expect(guarded.some((r) => r.selector.endsWith('::placeholder') && reaches(input, r))).toBe(true)
})

test('no rule of the old stylesheet that can match anywhere reaches an element a component draws', () => {
  drawEverything()
  const galleries = import.meta.glob<{ default: ComponentType; title: string }>('./*.gallery.tsx', { eager: true })
  render(
    Object.values(galleries).map(({ title, default: Specimens }) => (
      <section key={title}>
        <Specimens />
      </section>
    )),
  )
  const drawn = [...document.body.querySelectorAll('*')].filter(hasUi)
  expect(drawn.length).toBeGreaterThan(300)
  const kinds = new Set(drawn.map((el) => el.tagName.toLowerCase()))
  for (const tag of [
    'h1',
    'h2',
    'p',
    'a',
    'button',
    'input',
    'select',
    'textarea',
    'label',
    'table',
    'th',
    'td',
    'li',
    'ul',
    'hr',
    'dialog',
    'section',
    'nav',
  ]) {
    expect(kinds, tag).toContain(tag)
  }
  const reached: string[] = []
  for (const el of drawn) {
    for (const rule of guarded) {
      if (reaches(el, rule))
        reached.push(`${rule.selector} → <${el.tagName.toLowerCase()} class="${el.getAttribute('class')}">`)
    }
  }
  expect(reached).toEqual([])
})
