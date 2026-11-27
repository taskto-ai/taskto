import assert from 'node:assert/strict'
import { api, installExpenseTracker, owner, scenario, setUp } from '../support.mjs'

scenario(
  'in the rail every sidebar link and button has its name, the pinned apps too',
  async (s) => {
    const { page, context } = s
    await setUp(s)
    await installExpenseTracker(s)
    const { spaces } = await api(context, 'GET', '/api/workspaces/1/spaces')
    const space = spaces.find((x) => x.kind === 'private')
    const { apps } = await api(context, 'GET', `/api/workspaces/1/spaces/${space.id}/apps`)
    await api(context, 'POST', '/api/workspaces/1/home/items', { kind: 'pin', space_id: space.id, app_id: apps[0].id })
    await page.goto('/w/1/home')

    const nav = page.getByRole('navigation', { name: 'Sections' })
    await nav.getByRole('region', { name: 'Pinned apps' }).waitFor()
    const label = nav.locator('.ui-shell-link-label').first()
    assert.ok((await label.boundingBox()).width <= 1, 'the rail shows icons, not labels')

    const tree = await nav.ariaSnapshot()
    const links = tree.split('\n').filter((line) => /^\s*- link\b/.test(line))
    assert.ok(links.length >= 8, `the sections, the inbox, Settings and the pin: ${tree}`)
    for (const line of links) assert.match(line, /- link "[^"]+"/, `a link with no name: ${line}`)
    for (const name of ['Home', 'Conversations', 'Knowledge base', 'Boards', 'Apps', 'Notifications', 'Settings']) {
      assert.equal(await nav.getByRole('link', { name, exact: true }).count(), 1, name)
    }
    assert.equal(await nav.getByRole('region', { name: 'Pinned apps' }).getByRole('link').count(), 1)

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
    const main = page.getByRole('main')
    assert.equal(await main.locator('.settings-path').isVisible(), true)
    assert.equal((await main.locator('.settings-path').textContent()).trim(), 'Settings')
    await page.setViewportSize({ width: 1440, height: 900 })
    assert.equal(await main.locator('.settings-path').isVisible(), false)
  },
)

const LONG = 'Northwind'.repeat(9)

async function menuFacts(page) {
  const switcher = page.getByRole('button', { name: /^Workspace: / })
  await switcher.focus()
  await page.keyboard.press('Enter')
  const menu = page.getByRole('menu')
  await menu.waitFor()
  return menu.evaluate((list) => {
    const box = list.getBoundingClientRect()
    const checked = list.querySelector('[aria-checked="true"]')
    const check = checked.querySelector('.ui-menu-item-check')
    const c = check.getBoundingClientRect()
    const long = [...list.querySelectorAll('[role="menuitemradio"]')].find((i) => i.textContent.length > 50)
    const label = long.querySelector('.ui-menu-item-label').getBoundingClientRect()
    const item = long.getBoundingClientRect()
    const group = list.querySelector('.ui-menu-group')
    return {
      groupGap: group ? getComputedStyle(group).rowGap : null,
      sideways: list.scrollWidth - list.clientWidth,
      checkInside: c.left >= box.left && c.right <= box.right,
      checkColour: getComputedStyle(check).color === getComputedStyle(checked).color,
      wrapped: label.height > parseFloat(getComputedStyle(long).lineHeight) * 1.5,
      roomAbove: label.top - item.top,
      roomBelow: item.bottom - label.bottom,
    }
  })
}

for (const width of [390, 1440]) {
  scenario(
    `at ${width}px a long workspace name wraps inside the switcher's menu, and the check marks stay in sight`,
    async (s) => {
      const { page, context } = s
      const SIZE = { width, height: 900 }
      await setUp(s)
      await page.setViewportSize(SIZE)
      await api(context, 'POST', '/api/workspaces', { name: LONG })
      await page.goto('/w/1/conversations')
      if (width < 768) await page.getByRole('button', { name: 'Open the menu' }).click()
      const facts = await menuFacts(page)
      assert.equal(facts.sideways, 0, 'the menu does not scroll sideways')
      assert.ok(facts.checkInside, 'the check mark is inside the menu')
      assert.ok(facts.checkColour, "the check mark has the item's text colour, not the icons' muted one")
      assert.ok(facts.wrapped, 'the long name wraps')
      assert.equal(facts.groupGap, '1px', 'the gap between grouped items is the thin line')
      assert.ok(facts.roomAbove >= 7 && facts.roomBelow >= 7, `room around a wrapped item: ${JSON.stringify(facts)}`)
    },
  )
}

scenario(
  'in the rail nothing overflows sideways, the count sits on the bell’s corner, and the switcher has a hover ground',
  async (s) => {
    const { page } = s
    const SIZE = { width: 820, height: 1100 }
    await setUp(s)
    await page.setViewportSize(SIZE)
    await page.route('**/notifications/count', (r) => r.fulfill({ json: { unread: 150 } }))
    await page.goto('/w/1/conversations')
    const nav = page.getByRole('navigation', { name: 'Sections' })
    const count = nav.locator('.ui-shell-count')
    await count.waitFor()
    assert.equal(await count.textContent(), '99+')
    const facts = await page.evaluate(() => {
      const sidebar = document.querySelector('.ui-shell-sidebar')
      const pill = document.querySelector('.ui-shell-count').getBoundingClientRect()
      const bell = document.querySelector('.ui-shell-count').parentElement.querySelector('svg').getBoundingClientRect()
      return { overflow: sidebar.scrollWidth - sidebar.clientWidth, overTop: Math.max(0, pill.bottom - bell.top) }
    })
    assert.equal(facts.overflow, 0, 'the rail does not overflow sideways')
    assert.ok(facts.overTop <= 4, `the count covers ${facts.overTop}px of the bell`)
    const switcher = page.getByRole('button', { name: /^Workspace: / })
    await switcher.hover()
    await page.waitForTimeout(200)
    const ground = await switcher.evaluate((b) => getComputedStyle(b).backgroundColor)
    assert.notEqual(ground, 'rgba(0, 0, 0, 0)', 'a hovered switcher has a ground')
  },
)

for (const width of [390, 820]) {
  scenario(
    `at ${width}px, with a 20px root font, the box cut to the logo's mark is as wide as the mark is high`,
    async (s) => {
      const { page } = s
      const SIZE = { width, height: 900 }
      await setUp(s)
      await page.setViewportSize(SIZE)
      await page.goto('/w/1/conversations')
      await page.locator('.ui-shell').waitFor()
      await page.evaluate(() => document.documentElement.style.setProperty('font-size', '20px'))
      const { box, mark } = await page.evaluate(() => {
        const cut = document.querySelector(window.innerWidth <= 768 ? '.ui-shell-bar-mark' : '.ui-shell-logo')
        return { box: cut.getBoundingClientRect().width, mark: cut.querySelector('svg').getBoundingClientRect().height }
      })
      assert.ok(Math.abs(box - mark) < 0.5, `the box is ${box}px for a mark ${mark}px high`)
    },
  )
}

for (const dir of ['ltr', 'rtl']) {
  scenario(`at 320px (${dir}) the switcher's tooltip is never drawn over the switcher`, async (s) => {
    const { page, context } = s
    const SIZE = { width: 320, height: 800 }
    await setUp(s)
    await page.setViewportSize(SIZE)
    const made = await api(context, 'POST', '/api/workspaces', { name: 'Northwind Studio Design' })
    await page.goto(`/w/${made.workspace.id}/conversations`)
    await page.locator('.ui-shell').waitFor()
    await page.evaluate((d) => document.documentElement.setAttribute('dir', d), dir)
    await page.getByRole('button', { name: 'Open the menu' }).click()
    await page.waitForTimeout(500)
    await page.getByRole('button', { name: /^Workspace: / }).focus()
    await page.keyboard.press('Shift+Tab')
    await page.keyboard.press('Tab')
    const tip = page.locator('.ui-shell-switcher [role="tooltip"][data-open]')
    await tip.waitFor()
    const over = await page.evaluate(() => {
      const b = document.querySelector('.ui-shell-switcher-button').getBoundingClientRect()
      const r = document.querySelector('.ui-shell-switcher [role="tooltip"]').getBoundingClientRect()
      return !(r.bottom <= b.top || r.top >= b.bottom || r.right <= b.left || r.left >= b.right)
    })
    assert.equal(over, false, 'the tooltip is beside, above or below the switcher, not over it')
  })
}
