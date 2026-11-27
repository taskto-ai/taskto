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
