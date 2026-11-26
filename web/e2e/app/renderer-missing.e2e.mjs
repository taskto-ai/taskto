// The apps renderer's code is fetched when an app is drawn. When it doesn't arrive (a first visit
// before the service worker took over, a server reached over plain HTTP, a tab left open across an
// update), each app says so in its own frame with Try again, Home and the page carry on, and Try
// again (or a later draw elsewhere) fetches it again: no reload.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { api, scenario, setUp } from '../support.mjs'

const repo = join(dirname(fileURLToPath(import.meta.url)), '../../..')
const template = JSON.parse(readFileSync(join(repo, 'internal/apps/templates/expense-tracker.json'), 'utf8'))
const MISSING = /This screen couldn't be loaded/

scenario('a renderer that does not arrive is said in each app’s frame, and Try again fetches it again', async (s) => {
  const { page, context } = s
  await setUp(s)
  // Two apps with a widget each on Home, made through the API: the page hasn't loaded the renderer.
  const { spaces } = await api(context, 'GET', '/api/workspaces/1/spaces')
  const space = spaces.find((x) => x.kind === 'private')
  const base = `/api/workspaces/1/spaces/${space.id}/apps`
  const ids = []
  for (const name of ['Expenses A', 'Expenses B']) {
    const app = await api(context, 'POST', base, { name, definition: template.definition })
    await api(context, 'POST', `${base}/${app.id}/publish`, { version: app.version, acknowledge: [] })
    await api(context, 'POST', '/api/workspaces/1/home/items', {
      kind: 'widget',
      space_id: space.id,
      app_id: app.id,
      widget: 'spending',
    })
    ids.push(app.id)
  }
  // No cache and no service worker in the way: the renderer's code comes from the network, as on a
  // first visit before the worker took over or on a server reached over plain HTTP.
  const cdp = await context.newCDPSession(page)
  await cdp.send('Network.enable')
  await cdp.send('Network.setCacheDisabled', { cacheDisabled: true })
  await cdp.send('Network.setBypassServiceWorker', { bypass: true })
  let offline = true
  let asked = 0
  await page.route(/\/assets\/AppView-[^/]*\.js$/, (route) => {
    asked++
    return offline ? route.abort('internetdisconnected') : route.continue()
  })
  await page.goto('/w/1/home')

  // Home: each widget says it in its own frame; neither says the widget failed, and Home carries on.
  for (const name of ['Expenses A', 'Expenses B']) {
    const widget = page.getByRole('article').filter({ hasText: `From ${name}` })
    await widget.getByText(MISSING).waitFor()
    assert.equal(await widget.getByRole('button', { name: 'Try again' }).count(), 1)
  }
  assert.equal(await page.getByText('This widget couldn’t be shown.').count(), 0)
  assert.equal(await page.getByRole('heading', { level: 1, name: 'Home' }).count(), 1)
  assert.ok(asked > 0, 'the renderer was asked of the network')

  // An app opened: said in the app's frame, under the page's own head, not as the page's failure.
  await page.goto(`/w/1/apps/${space.id}-${ids[0]}`)
  await page.getByText(MISSING).waitFor()
  assert.equal(await page.getByRole('heading', { level: 1, name: 'Expenses A' }).count(), 1)
  assert.equal(await page.getByText(/This page couldn.t be shown/).count(), 0)

  // The connection is back: Try again draws the app, with no reload.
  offline = false
  const before = asked
  await page.getByRole('button', { name: 'Try again' }).click()
  await page.getByRole('button', { name: 'New expense' }).waitFor()
  assert.ok(asked > before, 'Try again fetched the renderer again')

  // And Home, drawn again by going there in the app (no reload), draws both widgets.
  await page.getByRole('navigation', { name: 'Sections' }).getByRole('link', { name: 'Home', exact: true }).click()
  for (const name of ['Expenses A', 'Expenses B']) {
    const widget = page.getByRole('article').filter({ hasText: `From ${name}` })
    await widget.getByText('This month').waitFor()
    assert.equal(await widget.getByText(MISSING).count(), 0)
  }
})
