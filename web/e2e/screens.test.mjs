// The screen list of `make screens` (screens.mjs) stays well formed. No browser and no server:
// the script only runs when it is the program being started, so importing it just lists screens.

import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { captures, PHASES, SCREENS, SETTINGS_TABS, SIZES, THEMES } from './screens.mjs'

describe('screens', () => {
  it('names every capture once, as a file that compares between runs', () => {
    const ids = captures().map((c) => c.id)
    expect(new Set(ids).size).toBe(ids.length)
    for (const id of ids) expect(id).toMatch(/^[a-z]+-(light|dark)\/[a-z0-9-]+$/)
  })

  it('has no screen name twice and only known phases', () => {
    const names = SCREENS.map((s) => s.name)
    expect(new Set(names).size).toBe(names.length)
    for (const s of SCREENS) expect(PHASES).toContain(s.phase)
  })

  it('captures at the three sizes in both themes', () => {
    expect(SIZES.map((s) => [s.width, s.height])).toEqual([
      [1440, 900],
      [820, 1100],
      [390, 844],
    ])
    expect(THEMES).toEqual(['light', 'dark'])
    const dirs = new Set(captures().map((c) => c.dir))
    expect(dirs.size).toBe(SIZES.length * THEMES.length)
  })

  it('goes through every tab of the settings page', () => {
    const source = readFileSync(new URL('../src/settings/SettingsPage.tsx', import.meta.url), 'utf8')
    const tabs = [...source.matchAll(/\{ tab: '([a-z-]+)', label:/g)].map((m) => m[1])
    expect(tabs.length).toBeGreaterThan(0)
    expect(SETTINGS_TABS).toEqual(tabs)
  })
})
