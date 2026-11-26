// The renderer's theme (ui/a2ui.css): every component the server's catalog lets an app use is
// themed, its controls get the library's focus ring and the touch height, and its colours come from
// the theme's roles, never from the renderer's own defaults.

import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, test } from 'vitest'
import goCatalog from '../../../internal/apps/testdata/catalog.json'
import { rules } from '../ui/cssValues'

const css = readFileSync(join(__dirname, '..', 'ui', 'a2ui.css'), 'utf8')
const all = rules(css)
const root = all.filter((r) => r.selector === ':root')

// What themes each renderer component an app may use: a variable of its own on :root, or a rule under
// .ui-app naming its element. taskto.ai's own components (drawn by catalog.tsx) are styled with the
// components and are not the renderer's.
const THEMED_BY: Record<string, string[]> = {
  Button: ['.a2ui-button'],
  Card: ['--a2ui-card-border'],
  CheckBox: ['a2ui-checkbox input', 'label.a2ui-checkbox'],
  ChoicePicker: ['.a2ui-chip', '.a2ui-option-input', '.a2ui-filter-input'],
  Column: ['--a2ui-column-gap'],
  DateTimeInput: ['.a2ui-date-time-input', '.a2ui-date-time-label'],
  Divider: ['--a2ui-divider-border'],
  Icon: ['--a2ui-icon-color'],
  List: ['--a2ui-list-gap'],
  Modal: ['.a2ui-modal-close', '--a2ui-modal-background'],
  Row: ['a2ui-basic-row'],
  Slider: ['.a2ui-slider', '.a2ui-slider-value'],
  Tabs: ['.a2ui-tab-button'],
  Text: ['.a2ui-text.a2ui-text-caption'],
  TextField: ['.a2ui-textfield', 'a2ui-basic-textfield > label'],
}
const OWN = ['BarcodeInput', 'LocationInput', 'PhotoInput', 'Photos', 'Screens', 'Show', 'SignatureInput', 'Stat']

// The renderer's controls: what a person focuses and presses.
const CONTROLS = [
  '.a2ui-button',
  '.a2ui-textfield',
  '.a2ui-date-time-input',
  '.a2ui-filter-input',
  '.a2ui-chip',
  '.a2ui-tab-button',
  '.a2ui-slider',
]

// The body of the first block whose prelude is this at-rule, braces balanced.
function block(at: string): string {
  const start = css.indexOf(at)
  if (start < 0) return ''
  let depth = 0
  for (let i = css.indexOf('{', start); i < css.length; i++) {
    if (css[i] === '{') depth++
    else if (css[i] === '}' && --depth === 0) return css.slice(css.indexOf('{', start) + 1, i)
  }
  return ''
}

describe("the renderer's theme", () => {
  test('every component of the catalog is either themed here or one of our own', () => {
    const components = Object.keys(goCatalog.components)
    expect(components.filter((c) => !(c in THEMED_BY) && !OWN.includes(c))).toEqual([])
    for (const [component, hooks] of Object.entries(THEMED_BY)) {
      for (const hook of hooks) {
        const found = hook.startsWith('--')
          ? root.some((r) => r.props.has(hook))
          : all.some((r) => r.selector.split(',').some((s) => s.trim().startsWith(`.ui-app ${hook}`)))
        expect(found, `${component}: ${hook}`).toBe(true)
      }
    }
  })

  test("every control gets the library's focus ring, not the renderer's change of border", () => {
    const focus = all.filter((r) => r.selector.includes(':focus-visible') && r.declarations.includes('outline'))
    const ringed = focus.flatMap((r) => r.selector.split(',').map((s) => s.trim()))
    for (const control of CONTROLS) expect(ringed, control).toContain(`.ui-app ${control}:focus-visible`)
  })

  test('on a touch screen every control is a finger’s height', () => {
    const coarse = block('@media (pointer: coarse)')
    for (const control of CONTROLS) expect(coarse, control).toContain(`.ui-app ${control}`)
    expect(coarse).toContain('var(--control-touch)')
  })

  test("its colours are the theme's roles, never the renderer's own", () => {
    const colours = [...root.flatMap((r) => [...r.props])].filter(
      ([name]) => /color|background|-bg$|border$/.test(name) && !name.startsWith('--a2ui-font'),
    )
    expect(colours.length).toBeGreaterThan(20)
    for (const [name, value] of colours) {
      // `none`: a line the theme draws another way (the tabs' hairline is a shadow, below).
      expect(value, name).toMatch(/var\(--color-[a-z-]+\)|currentColor|^none$/)
    }
  })

  test('the primary button is drawn as the library’s: an ink edge and the lift', () => {
    const primary = all.find((r) => r.selector === '.ui-app .a2ui-button.a2ui-button-primary')
    expect(primary?.declarations).toEqual(expect.arrayContaining(['border', 'box-shadow', 'font-weight']))
    const disabled = all.find((r) => r.selector.includes('.ui-app .a2ui-button:disabled'))
    // A designed state, not the renderer's half opacity.
    expect(disabled?.declarations).toEqual(expect.arrayContaining(['opacity', 'background', 'color']))
  })

  test('it is in no layer, so it can win over the renderer’s own rules, which are in none', () => {
    expect(all.every((r) => r.layer === undefined)).toBe(true)
    expect(css).not.toMatch(/@layer/)
  })
})

describe('the capture fields', () => {
  const capture = rules(readFileSync(join(__dirname, 'capture.css'), 'utf8'))
  const ruleOf = (selector: string) => capture.find((r) => r.selector.split(',').some((s) => s.trim() === selector))
  const declares = (selector: string, value: RegExp) => {
    const css = readFileSync(join(__dirname, 'capture.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '')
    const at = css.indexOf(`${selector} {`)
    return at >= 0 && value.test(css.slice(at, css.indexOf('}', at)))
  }

  test('their buttons are a finger’s size whatever the pointer', () => {
    expect(declares('.ui-capture .ui-button', /min-block-size: var\(--control-touch\)/)).toBe(true)
    expect(declares('.ui-capture .ui-icon-button', /min-inline-size: var\(--control-touch\)/)).toBe(true)
    expect(declares('.ui-photo-add > label', /min-block-size: var\(--control-touch\)/)).toBe(true)
    // The dictation's text buttons (Cancel, Try again) as well as its microphone.
    expect(declares('.ui-dictate .ui-button', /min-block-size: var\(--control-touch\)/)).toBe(true)
  })

  test('the pad and the saved signature are on paper, and the camera on media, in every theme', () => {
    expect(declares('.ui-signature-canvas', /background: var\(--color-paper\)/)).toBe(true)
    expect(declares('.ui-signature-image', /background: var\(--color-paper\)/)).toBe(true)
    expect(declares('.ui-capture-camera video', /background: var\(--color-media\)/)).toBe(true)
    expect(ruleOf('.ui-signature-canvas')?.layer).toBe('screens')
  })
})

// The declarations of the rule whose selector list holds exactly this selector, outside any at-rule,
// as text, for a check of what a rule sets.
function declarationsOf(source: string, selector: string): string {
  const text = source.replace(/\/\*[\s\S]*?\*\//g, '')
  const re = /([^{}]+)\{([^{}]*)\}/g
  for (let m = re.exec(text); m; m = re.exec(text)) {
    if (m[1].split(',').some((s) => s.trim() === selector)) return m[2]
  }
  return ''
}

describe('rings and what could cut them', () => {
  // Every control of the renderer a person can focus: each draws the library's ring.
  const RINGED = [
    '.a2ui-button',
    '.a2ui-textfield',
    '.a2ui-date-time-input',
    '.a2ui-filter-input',
    '.a2ui-chip',
    '.a2ui-tab-button',
    '.a2ui-slider',
    '.a2ui-modal-close',
    'a2ui-checkbox input',
    '.a2ui-option-input',
  ]

  test('every control the renderer draws has the ring', () => {
    for (const control of RINGED) {
      const rule = declarationsOf(css, `.ui-app ${control}:focus-visible`)
      expect(rule, control).toMatch(/outline: var\(--line-focus\) solid var\(--color-focus\)/)
    }
  })

  test('a tab’s ring is drawn inside it, and the row of tabs never scrolls up and down', () => {
    // The row scrolls sideways, which makes it clip: a ring outside a tab would be cut.
    expect(declarationsOf(css, '.ui-app .a2ui-tab-button:focus-visible')).toMatch(
      /outline-offset: calc\(-1 \* var\(--line-focus\)\)/,
    )
    const bar = declarationsOf(css, '.ui-app .a2ui-tab-bar')
    expect(bar).toMatch(/overflow-y: hidden/)
    // Its hairline is inside it, so the picked tab's underline needs no overlap the row would cut.
    expect(bar).toMatch(/box-shadow: inset/)
    expect(declarationsOf(css, '.ui-app .a2ui-tab-button')).toMatch(/margin-block-end: 0/)
  })

  test('a screen on a Home widget doesn’t scroll itself and leaves room for a ring and the lift', () => {
    // The frame's rules, which AppView and its lazy loader both draw (AppLoading.css).
    const view = readFileSync(join(__dirname, 'AppLoading.css'), 'utf8')
    const bare = declarationsOf(view, '.ui-app[data-frame="bare"]')
    // 8px: more than the ring's reach (2.5px wide, 2px off) and the primary's 3px lift.
    expect(bare).toMatch(/padding: var\(--space-2\)/)
    for (const selector of ['.ui-app[data-frame="bare"]', '.ui-app[data-frame]']) {
      expect(declarationsOf(view, selector), selector).not.toMatch(/overflow/)
    }
    // The panel scrolls sideways, inside the padding that leaves the same room.
    const panel = declarationsOf(view, '.ui-app[data-frame="panel"]')
    expect(panel).toMatch(/overflow-x: auto/)
    expect(panel).toMatch(/padding: var\(--space-4\)/)
  })

  test('the old stylesheet’s minimum height of an input reaches no field or check box of an app', () => {
    for (const selector of ['.ui-app .a2ui-textfield', '.ui-app a2ui-checkbox input']) {
      expect(declarationsOf(css, selector), selector).toMatch(/min-block-size: 0/)
    }
  })

  test('the modal’s close button keeps a colour of the theme on hover', () => {
    expect(declarationsOf(css, '.ui-app .a2ui-modal-close:hover')).toMatch(/color: var\(--color-text\)/)
  })
})
