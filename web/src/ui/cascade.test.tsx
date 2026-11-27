// @vitest-environment jsdom

import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, test } from 'vitest'
import { Checkbox } from './Checkbox'
import { Input } from './Input'

const dir = import.meta.dirname

const TOKENS = `:root {
  --color-danger: rgb(200, 0, 0);
  --color-border: rgb(1, 1, 1);
  --color-border-strong: rgb(2, 2, 2);
  --color-text: rgb(3, 3, 3);
  --color-text-muted: rgb(4, 4, 4);
  --color-focus: rgb(5, 5, 5);
  --color-hover: rgb(6, 6, 6);
  --line-focus: 2px;
  --line-thin: 1px;
  --line-strong: 2px;
}`

function load(file: string) {
  const css = readFileSync(join(dir, file), 'utf8')
    .replace(/:hover\b/g, '[data-hover]')
    .replace(/:focus-visible\b/g, '[data-focus-visible]')
  const style = document.createElement('style')
  style.textContent = TOKENS + css
  document.head.append(style)
  loaded.push(style)
}
const loaded: HTMLStyleElement[] = []

afterEach(() => {
  cleanup()
  for (const style of loaded.splice(0)) style.remove()
})

const edge = (el: Element) => getComputedStyle(el).borderTopColor

describe('the stylesheets, state against state', () => {
  test('a read-only field that is invalid keeps the danger edge', () => {
    load('Input.css')
    render(
      <>
        <Input aria-label="Invalid" invalid />
        <Input aria-label="Read-only" readOnly />
        <Input aria-label="Both" readOnly invalid />
      </>,
    )
    const invalid = edge(screen.getByLabelText('Invalid'))
    expect(invalid).not.toBe(edge(screen.getByLabelText('Read-only')))
    expect(edge(screen.getByLabelText('Both'))).toBe(invalid)
  })

  test('an invalid checkbox keeps the danger edge under the pointer', () => {
    load('Checkbox.css')
    render(
      <>
        <Checkbox label="Invalid" aria-invalid="true" />
        <Checkbox label="Hovered" />
      </>,
    )
    const invalid = screen.getByLabelText('Invalid')
    const hovered = screen.getByLabelText('Hovered')
    const danger = edge(invalid)
    const plain = edge(hovered)
    for (const box of [invalid, hovered]) box.closest('.ui-check')?.setAttribute('data-hover', '')
    expect(edge(hovered)).not.toBe(plain)
    expect(edge(invalid)).toBe(danger)
  })

  test("a menu item's keyboard focus has an outline, which forced colours keep", () => {
    load('Menu.css')
    render(
      <button type="button" className="ui-menu-item">
        Archive
      </button>,
    )
    const item = screen.getByRole('button', { name: 'Archive' })
    item.setAttribute('data-focus-visible', '')
    expect(getComputedStyle(item).outlineStyle).toBe('solid')
  })
})
