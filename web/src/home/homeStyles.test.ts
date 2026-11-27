// @vitest-environment jsdom

import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from 'vitest'

const css = readFileSync(join(__dirname, 'Home.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '')

function rules(): { selector: string; body: string; at: string[] }[] {
  const out: { selector: string; body: string; at: string[] }[] = []
  const stack: string[] = []
  let prelude = ''
  for (let i = 0; i < css.length; i++) {
    const c = css[i]
    if (c === '{') {
      const head = prelude.trim()
      prelude = ''
      if (head.startsWith('@')) {
        stack.push(head)
        continue
      }
      const end = css.indexOf('}', i)
      out.push({ selector: head, body: css.slice(i + 1, end), at: [...stack] })
      i = end
    } else if (c === '}') {
      stack.pop()
      prelude = ''
    } else prelude += c
  }
  return out
}

function matches(el: Element, selector: string): boolean {
  try {
    return el.matches(selector)
  } catch {
    return false
  }
}

function reaching(el: Element) {
  return rules().filter((r) => r.selector.split(',').some((s) => matches(el, s.trim())))
}

test('an app’s screen sits bare in its widget, with the room its focus ring and lift reach outside it', () => {
  document.body.innerHTML =
    '<div class="ui-home-widget-body"><section class="ui-app" data-frame="bare"></section></div>'
  const library = reaching(document.querySelector('section') as Element)
    .map((r) => r.body)
    .join(';')
  expect(library).not.toMatch(/padding/)
})

test('a medium widget spans two columns only where the grid has two, by the grid’s width, not the window’s', () => {
  document.body.innerHTML = '<ul class="ui-home-widgets"><li class="ui-home-cell" data-size="medium"></li></ul>'
  const cell = document.querySelector('li') as Element
  const spans = reaching(cell).filter((r) => /span 2/.test(r.body))
  expect(spans.length).toBeGreaterThan(0)
  for (const r of spans) {
    expect(r.at.some((a) => a.startsWith('@container'))).toBe(true)
    expect(r.at.some((a) => a.startsWith('@media'))).toBe(false)
  }
  expect(
    reaching(document.querySelector('ul') as Element).some((r) => /container-type:\s*inline-size/.test(r.body)),
  ).toBe(true)
})
