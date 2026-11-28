import assert from 'node:assert/strict'
import { api, owner, scenario, setUp } from '../support.mjs'

async function savedTabs(context, count) {
  for (let i = 0; i < 40; i++) {
    const { layout } = await api(context, 'GET', '/api/workspaces/1/desks/main')
    const tabs = JSON.stringify(layout).split('"path"').length - 1
    if (tabs === count) return layout
    await new Promise((resolve) => setTimeout(resolve, 250))
  }
  assert.fail(`the desk never held ${count} tabs`)
}

const tabNames = async (page) => (await page.getByRole('tab').allTextContents()).map((t) => t.trim())

scenario(
  'the desk keeps its panes and tabs across a reload and another browser, and is each person’s own',
  async (s) => {
    const { page, context, base } = s
    await setUp(s)
    await page.goto('/w/1/home')
    const nav = page.getByRole('navigation', { name: 'Sections' })
    await page.getByRole('tablist', { name: 'Tabs' }).waitFor()

    await nav.getByRole('link', { name: 'Boards', exact: true }).click()
    await page.getByRole('button', { name: 'New tab' }).click()
    await nav.getByRole('link', { name: 'Knowledge base', exact: true }).click()
    await page.getByRole('button', { name: 'Split to the side' }).first().click()
    await nav.getByRole('link', { name: 'Settings', exact: true }).click()
    assert.equal(await page.locator('.desk-pane').count(), 2)
    const built = await tabNames(page)
    assert.equal(built.length, 3)
    assert.equal(built[1], 'Knowledge base')
    await savedTabs(context, 3)
    const address = new URL(page.url()).pathname
    assert.equal(address, '/w/1/settings')

    await page.reload()
    await page.getByRole('tablist', { name: 'Tabs' }).first().waitFor()
    assert.equal(await page.locator('.desk-pane').count(), 2)
    const reloaded = ['Boards', built[1], built[2]]
    assert.deepEqual(await tabNames(page), reloaded)
    assert.equal(new URL(page.url()).pathname, address)

    await nav.getByRole('link', { name: 'Apps', exact: true }).click()
    await page.waitForFunction(() => location.pathname === '/w/1/apps')
    await page.waitForFunction(
      (was) => document.querySelectorAll('[role=tab]')[2]?.textContent?.trim() !== was,
      built[2],
    )
    assert.deepEqual((await tabNames(page)).slice(0, 2), reloaded.slice(0, 2))
    await page.goBack()
    await page.waitForFunction(() => location.pathname === '/w/1/settings')
    assert.deepEqual(await tabNames(page), reloaded)
    await page.getByRole('tab', { name: 'Boards', exact: true }).click()
    await page.waitForFunction(() => location.pathname.startsWith('/w/1/boards'))

    const again = await page
      .context()
      .browser()
      .newContext({ baseURL: base, viewport: { width: 1280, height: 900 } })
    const second = await again.newPage()
    await second.goto('/')
    await second.getByLabel('Email').fill(owner.email)
    await second.getByLabel('Password', { exact: true }).fill(owner.password)
    await second.getByRole('button', { name: 'Log in' }).click()
    await second.waitForFunction(() => document.querySelectorAll('.desk-pane').length === 2)
    assert.equal(await second.locator('.desk-pane').count(), 2)
    const seen = await tabNames(second)
    assert.ok(['Boards', 'Personal'].includes(seen[0]), seen[0])
    assert.deepEqual(seen.slice(1), reloaded.slice(1))

    const invite = await api(context, 'POST', '/api/workspaces/1/invites', { email: 'bob@example.com', role: 'member' })
    const bob = await page
      .context()
      .browser()
      .newContext({ baseURL: base, viewport: { width: 1280, height: 900 } })
    await api(bob, 'POST', '/api/invites/accept', { token: invite.token, name: 'Bob', password: owner.password })
    const theirs = await bob.newPage()
    await theirs.goto('/w/1/home')
    await theirs.getByRole('tablist', { name: 'Tabs' }).waitFor()
    assert.equal(await theirs.locator('.desk-pane').count(), 1)
    assert.deepEqual(await tabNames(theirs), ['Home'])
  },
  { viewport: { width: 1440, height: 900 } },
)

scenario('below the wide breakpoint the page is drawn alone, as before', async (s) => {
  const { page } = s
  await setUp(s)
  await page.setViewportSize({ width: 820, height: 1000 })
  await page.goto('/w/1/boards')
  await page.getByRole('main').getByRole('heading', { level: 1 }).first().waitFor()
  assert.equal(await page.getByRole('tablist', { name: 'Tabs' }).count(), 0)
})
