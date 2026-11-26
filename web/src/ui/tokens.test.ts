import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, test } from 'vitest'
import { THEME_COLOR } from '../theme'
import { type Ground, NON_TEXT_PAIRS, type PairSet, TEXT_PAIRS } from './contrast'
import { contrast, flatten, parseColor, propsOf, type Rule, resolve, rules, type Scheme } from './cssValues'
import { BRANDS, COLOR_ROLES, DEFAULT_BRAND } from './roles'

const dir = __dirname
const read = (name: string) => readFileSync(join(dir, name), 'utf8')
// Every stylesheet under src/, by its path there: the screens' files, the base, the shared rules and
// the design system's, not only styles.css, which only lists the screens' files.
const sheetPaths = () =>
  readdirSync(join(dir, '..'), { recursive: true, encoding: 'utf8' }).filter((f) => f.endsWith('.css'))
const allSheets = () => sheetPaths().map((f) => readFileSync(join(dir, '..', f), 'utf8'))
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
      // Every declaration, not only the custom properties: `color: red` in a brand is not a role.
      const declared = all[0].declarations
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
      // Nor a property that is not a custom one, which props (custom properties only) can't show.
      const roles = COLOR_ROLES.map((r) => `--color-${r}`) as string[]
      const others = rules(read(`themes/${brand}.css`))
        .flatMap((r) => r.declarations)
        .filter((name) => !roles.includes(name))
      expect(others, `${brand} sets more than colour roles`).toEqual([])
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
    // And the built CSS starts with it only if index.css is the app's first import: Vite writes CSS
    // in import order, and a component's `@layer components` block coming first would put that
    // layer below tokens, themes and base.
    const main = readFileSync(join(dir, '../main.tsx'), 'utf8')
    const imports = [...main.matchAll(/^import\s[^\n]*$/gm)].map((m) => m[0])
    expect(imports[0]).toBe("import './ui/index.css'")
  })

  test('the page declares the layers’ order before any stylesheet, as index.css does', () => {
    // A production build links the stylesheets of shared chunks (a component's `@layer components`)
    // before the entry's, so the order is fixed in the page itself, ahead of every <link> and <style>.
    // The page as it is: its comments name no element, so they need no stripping.
    const html = readFileSync(join(dir, '../../index.html'), 'utf8')
    const head = html.slice(0, html.indexOf('</head>'))
    const firstStyle = head.search(/<style\b|<link[^>]*rel="stylesheet"/)
    expect(firstStyle).toBeGreaterThan(-1)
    const style = /^<style>\s*([^<]*?)\s*<\/style>/.exec(head.slice(firstStyle))
    const index = read('index.css').replace(/\/\*[\s\S]*?\*\//g, '')
    expect(style?.[1]).toBe(index.trim().split('\n')[0])
  })

  test('the stacking levels are in their order, and every z-index is one of them', () => {
    const order = ['sticky', 'sidebar', 'scrim', 'drawer', 'overlay', 'toast', 'skip']
    const props = propsOf(rules(read('tokens.css')), ':root')
    const declared = [...props.keys()].filter((k) => k.startsWith('--z-'))
    expect(declared).toEqual(order.map((name) => `--z-${name}`))
    const levels = order.map((name) => Number(props.get(`--z-${name}`)))
    expect(
      levels.every((v, i) => Number.isInteger(v) && (i === 0 || v > levels[i - 1])),
      levels.join(' < '),
    ).toBe(true)
    // A level is a token: no stylesheet writes a z-index as a bare number, so none can climb over the
    // skip link without saying so in tokens.css.
    const numbers = allSheets().flatMap((css) => [...css.matchAll(/z-index:\s*(-?\d+)/g)].map((m) => m[0]))
    expect(numbers).toEqual([])
    const used = allSheets().flatMap((css) => [...css.matchAll(/z-index:\s*var\((--z-[a-z]+)\)/g)].map((m) => m[1]))
    expect(used.length).toBeGreaterThan(0)
    expect(used.filter((name) => !declared.includes(name))).toEqual([])
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
  'surface',
  'hover',
  'fg',
  'muted',
  'line',
  'line-strong',
  'accent',
  'on-accent',
  'accent-soft',
  'focus',
  'link',
  'logo-dot',
  'clear-icon',
  'font',
  'display',
  'mono',
  'radius',
  'touch',
]

// Their values before, resolved, for the colours: an alias must give the same colour.
const OLD_COLORS: Record<string, [string, string]> = {
  canvas: ['#faf8f4', '#111110'],
  surface: ['#ffffff', '#1b1b19'],
  hover: ['rgba(17, 17, 16, 0.05)', 'rgba(250, 248, 244, 0.07)'],
  fg: ['#111110', '#faf8f4'],
  muted: ['#5c5a55', '#aba89f'],
  line: ['rgba(17, 17, 16, 0.15)', 'rgba(250, 248, 244, 0.14)'],
  'line-strong': ['#7e7b74', '#8a877f'],
  accent: ['#4cefa8', '#4cefa8'],
  'on-accent': ['#111110', '#111110'],
  'accent-soft': ['#dcfceb', 'rgba(76, 239, 168, 0.14)'],
  focus: ['#111110', '#4cefa8'],
  link: ['#006b45', '#4cefa8'],
  'logo-dot': ['#00a86b', '#00a86b'],
}

// And the others, resolved as they were, light then dark: lengths, shadows, font stacks, the icon.
const SANS = '"Inter Variable", system-ui, -apple-system, "Segoe UI", Roboto, sans-serif'
const OLD_VALUES: Record<string, [string, string]> = {
  font: [SANS, SANS],
  display: [`"Bricolage Grotesque Variable", ${SANS}`, `"Bricolage Grotesque Variable", ${SANS}`],
  mono: [
    '"JetBrains Mono Variable", ui-monospace, "SF Mono", Menlo, Consolas, monospace',
    '"JetBrains Mono Variable", ui-monospace, "SF Mono", Menlo, Consolas, monospace',
  ],
  radius: ['8px', '8px'],
  touch: ['44px', '44px'],
  // The values base.css wrote as literals, which it takes from these aliases.
  'text-body': ['15px', '15px'],
  'text-h1': ['1.15rem', '1.15rem'],
  'text-h2': ['1rem', '1rem'],
  'text-h3': ['0.95rem', '0.95rem'],
  'text-code': ['0.9em', '0.9em'],
  'field-padding': ['0.45rem 0.6rem', '0.45rem 0.6rem'],
  'clear-icon': Array(2).fill(
    `url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Cpath d='M3.5 2.1 8 6.6l4.5-4.5 1.4 1.4L9.4 8l4.5 4.5-1.4 1.4L8 9.4l-4.5 4.5-1.4-1.4L6.6 8 2.1 3.5z'/%3E%3C/svg%3E")`,
  ) as [string, string],
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

  test('the other values are the ones they were', () => {
    for (const [name, [light, dark]] of Object.entries(OLD_VALUES)) {
      expect(resolve(`var(--tt-${name})`, props, 'light'), `--tt-${name} light`).toBe(light)
      expect(resolve(`var(--tt-${name})`, props, 'dark'), `--tt-${name} dark`).toBe(dark)
    }
  })

  test('every old name is pinned, as a colour or as a value', () => {
    const pinned = [...Object.keys(OLD_COLORS), ...Object.keys(OLD_VALUES)]
    expect(OLD_NAMES.filter((name) => !pinned.includes(name))).toEqual([])
  })

  test('every --tt-* name the stylesheets use without a fallback is defined', () => {
    // Set by script at run time (pwa/viewport.ts), not by a stylesheet.
    const runtime = new Set(['--tt-viewport-h', '--tt-viewport-top'])
    // The screens' files, the base and the shared rules: not only the list that imports them.
    expect(sheetPaths()).toEqual(expect.arrayContaining(['ui/base.css', 'legacy-shared.css', 'markdown.legacy.css']))
    const css = allSheets().join('\n')
    const missing = [...css.matchAll(/var\((--tt-[a-z0-9-]+)\s*\)/g)]
      .map((m) => m[1])
      .filter((n) => !runtime.has(n) && !props.has(n))
    expect([...new Set(missing)]).toEqual([])
  })
})

describe('reading the token files', () => {
  test('every declaration of a rule is recorded, not only the custom properties', () => {
    const [rule] = rules(':root[data-brand="x"] { --color-bg: #ffffff; color: red; accent-color: blue }')
    expect(rule.declarations).toEqual(['--color-bg', 'color', 'accent-color'])
    expect([...rule.props.keys()]).toEqual(['--color-bg'])
  })
})

describe('reading colours', () => {
  test('a channel or an alpha out of range is not a colour, so a theme cannot pass with one', () => {
    expect(() => parseColor('rgb(999, 999, 999)')).toThrow()
    expect(() => parseColor('rgba(0, 0, 0, 1.5)')).toThrow()
    expect(() => parseColor('rgb(-1, 0, 0)')).toThrow()
    expect(parseColor('rgba(255, 0, 0, 0.5)')).toEqual([255, 0, 0, 0.5])
  })
})
