import assert from 'node:assert/strict'
import { addExpense, api, installExpenseTracker, scenario, setUp } from '../support.mjs'

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
