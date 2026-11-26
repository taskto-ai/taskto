// The shell in a real browser, where only layout and the accessibility tree tell: on a tablet the
// sidebar is a rail of icons, and each of its links and buttons must still have its whole name.

import assert from 'node:assert/strict'
import { api, installExpenseTracker, owner, scenario, setUp } from '../support.mjs'

scenario(
  'in the rail every sidebar link and button has its name, the pinned apps too',
  async (s) => {
    const { page, context } = s
    await setUp(s)
    // A pinned app, so the rail has one of those too.
    await installExpenseTracker(s)
    const { spaces } = await api(context, 'GET', '/api/workspaces/1/spaces')
    const space = spaces.find((x) => x.kind === 'private')
    const { apps } = await api(context, 'GET', `/api/workspaces/1/spaces/${space.id}/apps`)
    await api(context, 'POST', '/api/workspaces/1/home/items', { kind: 'pin', space_id: space.id, app_id: apps[0].id })
    await page.goto('/w/1/home')

    const nav = page.getByRole('navigation', { name: 'Sections' })
    await nav.getByRole('region', { name: 'Pinned apps' }).waitFor()
    // The rail: the labels are out of sight.
    const label = nav.locator('.ui-shell-link-label').first()
    assert.ok((await label.boundingBox()).width <= 1, 'the rail shows icons, not labels')

    // Every link, as the accessibility tree has it: a name for each, the same as its label.
    const tree = await nav.ariaSnapshot()
    const links = tree.split('\n').filter((line) => /^\s*- link\b/.test(line))
    assert.ok(links.length >= 8, `the sections, the inbox, Settings and the pin: ${tree}`)
    for (const line of links) assert.match(line, /- link "[^"]+"/, `a link with no name: ${line}`)
    for (const name of ['Home', 'Conversations', 'Knowledge base', 'Boards', 'Apps', 'Notifications', 'Settings']) {
      assert.equal(await nav.getByRole('link', { name, exact: true }).count(), 1, name)
    }
    assert.equal(await nav.getByRole('region', { name: 'Pinned apps' }).getByRole('link').count(), 1)

    // The switcher and the account, which show only their avatars here, still name what they
    // hold: "Workspace: …" and "Account: …", not the label alone.
    const exact = true
    assert.equal(await page.getByRole('button', { name: `Workspace: ${owner.workspace}`, exact }).count(), 1)
    assert.equal(await page.getByRole('button', { name: `Account: ${owner.name} ${owner.email}`, exact }).count(), 1)
  },
  { viewport: { width: 820, height: 1100 } },
)

scenario(
  'on a phone, a link in the menu takes the keyboard to the new page; Settings says its name above the tab',
  async (s) => {
    const { page } = s
    await setUp(s)
    await page.setViewportSize({ width: 390, height: 844 })
    await page.goto('/w/1/home')
    await page.getByRole('heading', { level: 1 }).first().waitFor()
    for (const name of ['Boards', 'Settings']) {
      await page.getByRole('button', { name: 'Open the menu' }).click()
      await page.getByRole('navigation', { name: 'Sections' }).getByRole('link', { name, exact: true }).click()
      await page.waitForFunction(() => document.activeElement?.tagName === 'MAIN', null, { timeout: 5_000 })
    }
    // Settings on a phone: its title is the tab's, and "Settings" is the line above it.
    const main = page.getByRole('main')
    assert.equal(await main.locator('.settings-path').isVisible(), true)
    assert.equal((await main.locator('.settings-path').textContent()).trim(), 'Settings')
    // Wide, the side list's title says it, and the line is not drawn.
    await page.setViewportSize({ width: 1440, height: 900 })
    assert.equal(await main.locator('.settings-path').isVisible(), false)
  },
)
