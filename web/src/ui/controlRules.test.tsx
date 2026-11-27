// @vitest-environment jsdom

import { join } from 'node:path'
import { cleanup, render } from '@testing-library/react'
import type { ComponentType } from 'react'
import { afterEach, describe, expect, test } from 'vitest'
import { OLD_STYLESHEETS, oldStylesheets } from '../../scripts/css-check.mjs'
import { Button } from './Button'
import { Checkbox } from './Checkbox'
import { rules } from './cssValues'
import { IconButton } from './IconButton'
import { Input } from './Input'
import { SegmentedControl } from './SegmentedControl'
import { Select } from './Select'
import { Switch } from './Switch'
import { Textarea } from './Textarea'

afterEach(() => {
  cleanup()
  document.body.innerHTML = ''
})

const sheets = oldStylesheets(join(__dirname, '../..'))
const css = sheets.map((s) => s.css).join('\n')

function split(list: string): string[] {
  const out: string[] = []
  let depth = 0
  let cur = ''
  for (const c of list) {
    if (c === '(') depth++
    if (c === ')') depth--
    if (c === ',' && depth === 0) {
      out.push(cur.trim())
      cur = ''
    } else cur += c
  }
  out.push(cur.trim())
  return out.filter(Boolean)
}

const selectors = rules(css).flatMap((r) => split(r.selector))

const EVERY_FIELD = new Set(['input', 'select', 'textarea'])
const elementSelectors = selectors.filter((s) => /^[a-z]/.test(s) && !s.includes('::') && !EVERY_FIELD.has(s))

const galleries = import.meta.glob<{ default: ComponentType; title: string }>('./*.gallery.tsx', { eager: true })

describe('the old stylesheet and the controls', () => {
  test("it reads the old stylesheets themselves: the base and every screen's file", () => {
    expect(sheets.map((s) => s.file)).toEqual(OLD_STYLESHEETS.map((f) => `src/${f}`))
  })

  test('it has element rules, and they still reach the screens’ own elements', () => {
    expect(elementSelectors.length).toBeGreaterThan(10)
    document.body.innerHTML =
      '<label><input type="checkbox"></label><label><input type="radio"></label><button>b</button><select></select><a href="#">a</a>'
    for (const el of document.body.querySelectorAll('input, label, button, select, a')) {
      expect(
        elementSelectors.some((s) => el.matches(s)),
        el.outerHTML,
      ).toBe(true)
    }
  })

  test('no element rule of it, in any media block, reaches an element a control draws', () => {
    render(
      Object.values(galleries).map(({ title, default: Specimens }) => (
        <section key={title}>
          <Specimens />
        </section>
      )),
    )
    const drawn = [...document.querySelectorAll<HTMLElement>('[class^="ui-"], [class*=" ui-"]')]
    const kinds = new Set(
      drawn.map((el) =>
        el.tagName === 'INPUT' ? `input:${el.getAttribute('type') ?? 'text'}` : el.tagName.toLowerCase(),
      ),
    )
    for (const kind of ['button', 'a', 'input:checkbox', 'input:radio', 'input:text', 'select', 'textarea', 'label']) {
      expect(kinds, kind).toContain(kind)
    }
    const reached: string[] = []
    for (const el of drawn) {
      for (const s of elementSelectors) {
        if (el.matches(s)) reached.push(`${s} → <${el.tagName.toLowerCase()} class="${el.className}">`)
      }
    }
    expect(reached).toEqual([])
  })

  test('the opt-out adds no specificity: a ui- class is named only inside :where()', () => {
    const outside = (s: string) => {
      let out = s
      for (;;) {
        const at = out.indexOf(':where(')
        if (at < 0) return out
        let depth = 0
        let end = at + ':where'.length
        for (; end < out.length; end++) {
          if (out[end] === '(') depth++
          if (out[end] === ')' && --depth === 0) break
        }
        out = out.slice(0, at) + out.slice(end + 1)
      }
    }
    expect(selectors.filter((s) => /(^|[^\w-])ui-/.test(outside(s)))).toEqual([])
  })
})

describe('every control marks its native element', () => {
  test('with ui-control', () => {
    render(
      <>
        <Button>b</Button>
        <IconButton icon="edit" label="e" />
        <Input aria-label="i" />
        <Textarea aria-label="t" />
        <Select aria-label="s">
          <option>o</option>
        </Select>
        <Checkbox label="c" />
        <Switch label="w" />
        <SegmentedControl
          label="g"
          value="a"
          onChange={() => {}}
          options={[
            { value: 'a', label: 'A' },
            { value: 'b', label: 'B' },
          ]}
        />
      </>,
    )
    const natives = document.querySelectorAll('button, input, select, textarea')
    expect(natives.length).toBe(9)
    for (const el of natives) expect(el.classList.contains('ui-control'), el.outerHTML).toBe(true)
  })
})
