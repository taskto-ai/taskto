import assert from 'node:assert/strict'
import { addExpense, api, installExpenseTracker, owner, scenario, setUp } from '../support.mjs'

scenario('a template is installed, published, and a record is added to it', async (s) => {
  const { page, context } = s
  await setUp(s)
  const app = await installExpenseTracker(s)

  assert.deepEqual((await api(context, 'GET', app.records)).records, [])

  await addExpense(page, { what: 'Team lunch', amount: 42 })
  await page.getByText('Team lunch').waitFor()

  const { records } = await api(context, 'GET', app.records)
  assert.equal(records.length, 1)
  assert.equal(records[0].data.description, 'Team lunch')
  assert.equal(Number(records[0].data.amount), 42)
  assert.equal(records[0].created_by.name, 'Owner')
  await page.reload()
  await page.getByText('Team lunch').waitFor()
})

scenario(
  'an app opens on its own in a window and at its address, signed in, and is not found by someone else',
  async (s) => {
    const { page, context } = s
    await setUp(s)
    const { records } = await installExpenseTracker(s)
    const [, spaceId, appId] = /spaces\/(\d+)\/apps\/(\d+)\//.exec(records)
    const run = `/w/1/run/${spaceId}-${appId}`
    await addExpense(page, { what: 'Team lunch', amount: 42 })
    await page.getByText('Team lunch').waitFor()

    const [popup] = await Promise.all([
      page.waitForEvent('popup'),
      page.getByRole('button', { name: 'Open in a new window' }).click(),
    ])
    await popup.getByRole('button', { name: 'New expense' }).waitFor()
    assert.equal(new URL(popup.url()).pathname, run)
    assert.equal(await popup.getByRole('navigation', { name: 'Sections' }).count(), 0)
    assert.equal(await popup.getByRole('link', { name: 'All apps' }).count(), 0)
    assert.equal(await popup.title(), 'Expense tracker · taskto.ai')
    await popup.getByText('Team lunch').waitFor()
    assert.equal(await popup.evaluate(() => window.opener), null)
    assert.equal(await popup.evaluate(() => window.name), `taskto-app-${spaceId}-${appId}`)

    const windows = context.pages().length
    let another = false
    context.on('page', () => {
      another = true
    })
    await page.getByRole('button', { name: 'Open in a new window' }).click()
    await popup.waitForURL(`**${run}`)
    await popup.getByRole('button', { name: 'New expense' }).waitFor()
    assert.equal(another, false, 'a second click opens no second window')
    assert.equal(context.pages().length, windows)
    assert.equal(await popup.evaluate(() => window.opener), null)

    const elsewhere = new URL(page.url())
    elsewhere.hostname = 'localhost'
    elsewhere.pathname = '/healthz'
    await popup.goto(elsewhere.href)
    const [reopened] = await Promise.all([
      page.waitForEvent('popup'),
      page.getByRole('button', { name: 'Open in a new window' }).click(),
    ])
    await reopened.getByRole('button', { name: 'New expense' }).waitFor()
    assert.equal(new URL(reopened.url()).pathname, run)
    assert.equal(await reopened.evaluate(() => window.opener), null)
    assert.equal(new URL(popup.url()).pathname, '/healthz', 'the window that left the app is not touched')
    await popup.close()
    await page.getByRole('button', { name: 'Open in a new window' }).click()
    await reopened.waitForURL(`**${run}`)
    assert.equal(context.pages().length, windows, 'the new window is the one that is reused')
    assert.equal(await reopened.evaluate(() => window.opener), null)
    await reopened.close()

    await page.goto(run)
    await page.getByRole('heading', { level: 1, name: 'Expense tracker' }).waitFor()
    await page.getByRole('button', { name: 'New expense' }).waitFor()
    assert.equal(await page.getByRole('navigation', { name: 'Sections' }).count(), 0)
    assert.equal(new URL(page.url()).pathname, run)
    await page.getByRole('link', { name: 'Open in taskto' }).click()
    await page.getByRole('navigation', { name: 'Sections' }).waitFor()
    assert.equal(new URL(page.url()).pathname, `/w/1/apps/${spaceId}-${appId}`)

    const invite = await api(context, 'POST', '/api/workspaces/1/invites', { email: 'bob@example.com', role: 'member' })
    await context.clearCookies()
    await page.goto(run)
    await page.getByRole('heading', { name: 'Log in' }).waitFor()
    await page.getByLabel('Email').fill(owner.email)
    await page.getByLabel('Password', { exact: true }).fill(owner.password)
    await page.getByRole('button', { name: 'Log in' }).click()
    await page.getByRole('button', { name: 'New expense' }).waitFor()
    assert.equal(new URL(page.url()).pathname, run)
    assert.equal(await page.getByRole('navigation', { name: 'Sections' }).count(), 0)

    await context.clearCookies()
    await api(context, 'POST', '/api/invites/accept', { token: invite.token, name: 'Bob', password: owner.password })
    await page.goto(run)
    await page.getByText('This app wasn’t found').waitFor()
    assert.equal(await page.getByText('Team lunch').count(), 0)
    assert.equal(await page.getByRole('button', { name: 'New expense' }).count(), 0)
    assert.equal(new URL(page.url()).pathname, run)
    const refused = await context.request.get(records)
    assert.ok(!refused.ok(), 'the server refuses the other person the records')
  },
)

scenario(
  'a template whose draft declares an integration slot is not published without approval, and every other template is',
  async (s) => {
    const { context } = s
    await setUp(s)
    const { spaces } = await api(context, 'GET', '/api/workspaces/1/spaces')
    const space = spaces.find((x) => x.kind === 'private')
    const spacePath = `/api/workspaces/1/spaces/${space.id}`
    const { templates } = await api(context, 'GET', '/api/workspaces/1/app-templates')
    const declared = []
    const plain = []
    for (const template of templates.filter((t) => !t.needs_board)) {
      const app = await api(context, 'POST', `${spacePath}/app-templates/${template.id}/install`, {})
      const slots = Object.keys(app.draft?.integrations ?? {})
      const res = await context.request.fetch(`${spacePath}/apps/${app.id}/publish`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        data: JSON.stringify({ version: app.version, acknowledge: [] }),
      })
      if (slots.length > 0) {
        assert.equal(res.status(), 409, `${template.id} declares ${slots} and was published`)
        assert.equal((await res.json()).code, 'app_integration_unapproved')
        declared.push(template.id)
      } else {
        assert.ok(res.ok(), `${template.id} declares no slot and was refused: ${res.status()} ${await res.text()}`)
        plain.push(template.id)
      }
    }
    assert.ok(declared.length > 0, 'no bundled template declares an integration slot any more: review the screens seed')
    assert.ok(plain.length > 0, 'no bundled template publishes without approval')
  },
)

scenario('an icon of joined pictographs stays inside its avatar in the list of apps', async (s) => {
  const { page, context } = s
  await setUp(s)
  const { spaces } = await api(context, 'GET', '/api/workspaces/1/spaces')
  const space = spaces.find((x) => x.kind === 'private')
  const spacePath = `/api/workspaces/1/spaces/${space.id}`
  const { templates } = await api(context, 'GET', '/api/workspaces/1/app-templates')
  const template = templates.find((t) => t.name === 'Expense tracker')
  const app = await api(context, 'POST', `${spacePath}/app-templates/${template.id}/install`, {})
  const joined = '🧾‍🧾‍🧾‍🧾🏻'
  await api(context, 'PATCH', `${spacePath}/apps/${app.id}`, { version: app.version, icon: joined })

  await page.getByRole('link', { name: 'Apps', exact: true }).click()
  const avatar = page.locator('.apps-avatar-icon').first()
  await avatar.waitFor()
  const measured = await avatar.evaluate((el) => {
    const range = document.createRange()
    range.selectNodeContents(el)
    return {
      overflow: getComputedStyle(el).overflow,
      box: el.getBoundingClientRect().width,
      glyphs: range.getBoundingClientRect().width,
    }
  })
  assert.ok(
    measured.glyphs > measured.box,
    `the icon is not wider than its avatar (${JSON.stringify(measured)}): the test no longer exercises the overflow`,
  )
  assert.equal(measured.overflow, 'hidden')
  const before = await avatar.boundingBox()
  assert.ok(before.width <= 44, `the avatar grew to ${before.width}px`)
})
