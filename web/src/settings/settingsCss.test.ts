// The settings' stylesheet keeps the page inside the window on a phone. A grid whose column is the
// default (`auto`) grows to its widest child's minimum width, so a table inside it widens the whole
// tab instead of scrolling in its own box. Every grid here says its columns, and a single column is
// `minmax(0, 1fr)`, which may shrink below its content.

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { expect, test } from 'vitest'

const css = readFileSync(resolve(__dirname, 'Settings.css'), 'utf8')

// Each rule's selector and body, for the simple flat rules this file has.
const rules = [...css.matchAll(/([^{}@]+)\{([^{}]*)\}/g)].map((m) => ({
  selector: m[1].replace(/\/\*[\s\S]*?\*\//g, '').trim(),
  body: m[2],
}))

test('every grid in the settings says its columns, so none grows wider than its place', () => {
  const grids = rules.filter((r) => /display:\s*grid/.test(r.body))
  expect(grids.length).toBeGreaterThan(10)
  const unsized = grids.filter((r) => !/grid-template-columns:/.test(r.body)).map((r) => r.selector)
  expect(unsized).toEqual([])
})

test('the sections, panels and forms that hold a table or a long row may shrink below it', () => {
  for (const name of ['.settings-section', '.settings-panel-body', '.settings-form']) {
    const rule = rules.find((r) => r.selector === name)
    expect(rule?.body).toMatch(/grid-template-columns:\s*minmax\(0,\s*1fr\)/)
  }
})
