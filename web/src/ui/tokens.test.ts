import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, test } from 'vitest'
import { THEME_COLOR } from '../theme'
import { type Ground, NON_TEXT_PAIRS, type PairSet, TEXT_PAIRS } from './contrast'
import { contrast, flatten, parseColor, propsOf, type Rule, resolve, rules, type Scheme } from './cssValues'
import { BRANDS, COLOR_ROLES, DEFAULT_BRAND } from './roles'

const dir = __dirname
const read = (name: string) => readFileSync(join(dir, name), 'utf8')
const themeFiles = readdirSync(join(dir, 'themes')).filter((f) => f.endsWith('.css'))

const selectorOf = (brand: string) => (brand === DEFAULT_BRAND ? ':root' : `:root[data-brand="${brand}"]`)

// Every property the page has on :root with this brand: the scales, the default brand's roles
// and then this brand's over them, the old names and the renderer's.
function pageProps(brand: string): Map<string, string> {
  const all: Rule[] = [
    ...rules(read('tokens.css')),
    ...rules(read(`themes/${DEFAULT_BRAND}.css`)),
    ...rules(read('legacy.css')),
    ...rules(read('a2ui.css')),
  ]
  const props = propsOf(all, ':root')
  if (brand !== DEFAULT_BRAND) {
    for (const [k, v] of propsOf(rules(read(`themes/${brand}.css`)), selectorOf(brand))) props.set(k, v)
  }
  return props
}

// A brand's own roles, from its file alone: nothing inherited from the default.
function brandRoles(brand: string): Map<string, string> {
  return propsOf(rules(read(`themes/${brand}.css`)), selectorOf(brand))
}

function colorOf(role: string, props: Map<string, string>, scheme: Scheme) {
  return parseColor(resolve(`var(--color-${role})`, props, scheme))
}

function groundOf(ground: Ground, props: Map<string, string>, scheme: Scheme) {
  const stack = typeof ground === 'string' ? [ground] : ground
  return flatten(stack.map((role) => colorOf(role, props, scheme)))
}

const groundName = (g: Ground) => (typeof g === 'string' ? g : g.join(' over '))

// Every pair of a list that falls under its minimum, as "brand scheme: fg on ground ratio".
function failures(pairs: readonly PairSet[], brand: string, props: Map<string, string>): string[] {
  const out: string[] = []
  for (const scheme of ['light', 'dark'] as const) {
    for (const set of pairs) {
      for (const fg of set.fg) {
        for (const on of set.on) {
          const ratio = contrast(colorOf(fg, props, scheme), groundOf(on, props, scheme))
          if (ratio < set.min)
            out.push(`${brand} ${scheme}: ${fg} on ${groundName(on)} is ${ratio.toFixed(2)}, under ${set.min}`)
        }
      }
    }
  }
  return out
}

describe('the brand themes', () => {
  test('the theme files are the brands, and the default one is the one the app loads', () => {
    expect(themeFiles.map((f) => f.replace(/\.css$/, '')).sort()).toEqual([...BRANDS].sort())
    expect(read('index.css')).toContain(`@import "./themes/${DEFAULT_BRAND}.css";`)
    for (const brand of BRANDS.filter((b) => b !== DEFAULT_BRAND)) {
      expect(read('index.css')).not.toContain(`themes/${brand}.css`)
    }
  })

  for (const brand of BRANDS) {
    test(`${brand} is one block in the themes layer that sets every colour role and nothing else`, () => {
      const all = rules(read(`themes/${brand}.css`))
      expect(all.map((r) => [r.selector, r.layer])).toEqual([[selectorOf(brand), 'themes']])
      const declared = [...all[0].props.keys()]
      const expected = COLOR_ROLES.map((r) => `--color-${r}`)
      expect(
        expected.filter((name) => !declared.includes(name)),
        'roles missing',
      ).toEqual([])
      expect(
        declared.filter((name) => !expected.includes(name)),
        'not colour roles',
      ).toEqual([])
    })

    test(`every role of ${brand} is a colour in light and in dark`, () => {
      const props = pageProps(brand)
      for (const scheme of ['light', 'dark'] as const) {
        for (const role of COLOR_ROLES) expect(() => colorOf(role, props, scheme), role).not.toThrow()
      }
    })

    test(`every text pair of ${brand} is at least 4.5:1, in light and in dark`, () => {
      expect(failures(TEXT_PAIRS, brand, pageProps(brand))).toEqual([])
    })

    test(`every control edge, the focus ring and the column marks of ${brand} are at least 3:1`, () => {
      expect(failures(NON_TEXT_PAIRS, brand, pageProps(brand))).toEqual([])
    })
  }

  test('a second brand changes colour roles and nothing else', () => {
    const base = pageProps(DEFAULT_BRAND)
    for (const brand of BRANDS.filter((b) => b !== DEFAULT_BRAND)) {
      const swapped = pageProps(brand)
      const changed = [...swapped].filter(([k, v]) => base.get(k) !== v).map(([k]) => k)
      expect(changed.length, `${brand} changes no role`).toBeGreaterThan(0)
      expect(changed.filter((k) => !k.startsWith('--color-'))).toEqual([])
      expect([...brandRoles(brand).keys()].sort()).toEqual(COLOR_ROLES.map((r) => `--color-${r}`).sort())
    }
  })

  test("the page's ground is THEME_COLOR, which the metas and the manifest hold to", () => {
    const props = pageProps(DEFAULT_BRAND)
    expect(resolve('var(--color-bg)', props, 'light')).toBe(THEME_COLOR.light)
    expect(resolve('var(--color-bg)', props, 'dark')).toBe(THEME_COLOR.dark)
  })
})

describe('the tokens', () => {
  test('the scales set no colour role, and a colour role is set only by a theme', () => {
    for (const file of ['tokens.css', 'legacy.css', 'a2ui.css']) {
      const names = rules(read(file)).flatMap((r) => [...r.props.keys()])
      expect(
        names.filter((n) => n.startsWith('--color-')),
        file,
      ).toEqual([])
    }
  })

  test('every colour role the stylesheets use exists', () => {
    const used = new Set<string>()
    const walk = (d: string) => {
      for (const e of readdirSync(d, { withFileTypes: true })) {
        if (e.isDirectory()) walk(join(d, e.name))
        else if (e.name.endsWith('.css')) {
          for (const m of readFileSync(join(d, e.name), 'utf8').matchAll(/var\(--color-([a-z0-9-]+)/g)) used.add(m[1])
        }
      }
    }
    walk(join(dir, '..'))
    expect([...used].filter((r) => !(COLOR_ROLES as readonly string[]).includes(r))).toEqual([])
  })

  test('the layers are declared once, in order, before anything is imported', () => {
    const index = read('index.css').replace(/\/\*[\s\S]*?\*\//g, '')
    expect(index.trim().split('\n')[0]).toBe('@layer tokens, themes, base, components, screens;')
  })

  test('every scale resolves', () => {
    const props = pageProps(DEFAULT_BRAND)
    for (const [name] of propsOf(rules(read('tokens.css')), ':root')) {
      for (const scheme of ['light', 'dark'] as const)
        expect(() => resolve(`var(${name})`, props, scheme), name).not.toThrow()
    }
  })
})

// The names the stylesheet had before the tokens had two layers. They stay until the screens move.
const OLD_NAMES = [
  'canvas',
  'stone',
  'surface',
  'hover',
  'selected',
  'user',
  'fg',
  'fg-soft',
  'muted',
  'line',
  'rule',
  'line-strong',
  'accent',
  'accent-hover',
  'on-accent',
  'accent-edge',
  'accent-soft',
  'focus',
  'link',
  'node',
  'node-edge',
  'logo-dot',
  'bar',
  'on-bar',
  'on-bar-muted',
  'danger',
  'on-danger',
  'success',
  'warning',
  'clear-icon',
  'qr-ground',
  'scrim',
  'shadow-lift',
  'shadow-lift-hover',
  'shadow-pop',
  'font',
  'display',
  'mono',
  'radius',
  'topbar-h',
  'sidebar-w',
  'touch',
]

// Their values before, resolved, for the colours: an alias must give the same colour.
const OLD_COLORS: Record<string, [string, string]> = {
  canvas: ['#faf8f4', '#111110'],
  stone: ['#f1efe9', '#181816'],
  surface: ['#ffffff', '#1b1b19'],
  hover: ['rgba(17, 17, 16, 0.05)', 'rgba(250, 248, 244, 0.07)'],
  selected: ['rgba(17, 17, 16, 0.08)', 'rgba(250, 248, 244, 0.12)'],
  user: ['#f1efe9', '#262624'],
  fg: ['#111110', '#faf8f4'],
  'fg-soft': ['#2e2d2a', '#d6d3cc'],
  muted: ['#5c5a55', '#aba89f'],
  line: ['rgba(17, 17, 16, 0.15)', 'rgba(250, 248, 244, 0.14)'],
  rule: ['#111110', 'rgba(250, 248, 244, 0.24)'],
  'line-strong': ['#7e7b74', '#8a877f'],
  accent: ['#4cefa8', '#4cefa8'],
  'accent-hover': ['#6bf5b8', '#6bf5b8'],
  'on-accent': ['#111110', '#111110'],
  'accent-edge': ['#111110', '#1fc98a'],
  'accent-soft': ['#dcfceb', 'rgba(76, 239, 168, 0.14)'],
  focus: ['#111110', '#4cefa8'],
  link: ['#006b45', '#4cefa8'],
  node: ['#4cefa8', '#4cefa8'],
  'node-edge': ['#1fc98a', '#1fc98a'],
  'logo-dot': ['#00a86b', '#00a86b'],
  bar: ['#faf8f4', '#111110'],
  'on-bar': ['#111110', '#faf8f4'],
  'on-bar-muted': ['#5c5a55', '#aba89f'],
  danger: ['#b42318', '#ff8a82'],
  'on-danger': ['#ffffff', '#111110'],
  success: ['#006b45', '#4cefa8'],
  warning: ['#9a4700', '#ffb04d'],
  'qr-ground': ['#ffffff', '#ffffff'],
  scrim: ['rgba(17, 17, 16, 0.42)', 'rgba(0, 0, 0, 0.6)'],
}

describe('the old --tt-* names', () => {
  const props = pageProps(DEFAULT_BRAND)

  test('all still resolve, in light and in dark', () => {
    for (const name of OLD_NAMES) {
      for (const scheme of ['light', 'dark'] as const) {
        expect(() => resolve(`var(--tt-${name})`, props, scheme), `--tt-${name}`).not.toThrow()
      }
    }
  })

  test('the colours are the ones they were', () => {
    for (const [name, [light, dark]] of Object.entries(OLD_COLORS)) {
      expect(parseColor(resolve(`var(--tt-${name})`, props, 'light')), `--tt-${name} light`).toEqual(parseColor(light))
      expect(parseColor(resolve(`var(--tt-${name})`, props, 'dark')), `--tt-${name} dark`).toEqual(parseColor(dark))
    }
  })

  test('every --tt-* name the stylesheet uses without a fallback is defined', () => {
    // Set by script at run time (pwa/viewport.ts), not by a stylesheet.
    const runtime = new Set(['--tt-viewport-h', '--tt-viewport-top'])
    const css = readFileSync(join(dir, '../styles.css'), 'utf8')
    const missing = [...css.matchAll(/var\((--tt-[a-z0-9-]+)\s*\)/g)]
      .map((m) => m[1])
      .filter((n) => !runtime.has(n) && !props.has(n))
    expect([...new Set(missing)]).toEqual([])
  })
})
