// The shell in a real browser, where only layout and the accessibility tree tell: on a tablet the
// sidebar is a rail of icons, and each of its links must still have a name.

import assert from 'node:assert/strict'
import { api, installExpenseTracker, scenario, setUp } from '../support.mjs'

scenario(
  'in the rail every sidebar link has a name, the pinned apps too',
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
  },
  { viewport: { width: 820, height: 1100 } },
)
