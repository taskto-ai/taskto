import assert from 'node:assert/strict'
import { addExpense, api, installExpenseTracker, scenario, setUp } from '../support.mjs'

scenario('a record added offline is kept on the device and syncs when back online', async (s) => {
  const { page, context } = s
  await setUp(s)
  const app = await installExpenseTracker(s)

  await page.getByRole('button', { name: 'Download Expense tracker for offline use' }).click()
  await page.getByText('Ready for offline use').waitFor()

  await page.waitForFunction(() => navigator.serviceWorker.ready.then(() => true), null, { timeout: 15_000 })
  await context.setOffline(true)
  await page.reload()
  await page.getByRole('heading', { name: 'Offline', exact: true }).waitFor()
  await page.getByRole('button', { name: 'Open Expense tracker' }).click()

  await addExpense(page, { what: 'Train ticket', amount: 18 })
  await page.getByText('Saved on this device').first().waitFor()
  await page.getByText('Train ticket').first().waitFor()

  await page.reload()
  await page.getByRole('heading', { name: 'Offline', exact: true }).waitFor()
  await page.getByRole('button', { name: 'Open Expense tracker' }).click()
  await page.getByText('Train ticket').first().waitFor()
  await page.getByText('Saved on this device, waiting to sync').first().waitFor()

  await context.setOffline(false)
  await page.getByText('Synced', { exact: true }).first().waitFor()
  const { records } = await api(context, 'GET', app.records)
  assert.equal(records.length, 1)
  assert.equal(records[0].data.description, 'Train ticket')
  assert.equal(Number(records[0].data.amount), 18)
  assert.equal(records[0].created_by.name, 'Owner')
})

scenario('a downloaded app’s own address opens that app when the page is loaded with no connection', async (s) => {
  const { page, context } = s
  await setUp(s)
  const { records } = await installExpenseTracker(s)
  const [, spaceId, appId] = /spaces\/(\d+)\/apps\/(\d+)\//.exec(records)
  const run = `/w/1/run/${spaceId}-${appId}`

  await page.getByRole('button', { name: 'Download Expense tracker for offline use' }).click()
  await page.getByText('Ready for offline use').waitFor()
  await page.waitForFunction(() => navigator.serviceWorker.ready.then(() => true), null, { timeout: 15_000 })

  await context.setOffline(true)
  await page.goto(run)
  await page.getByRole('heading', { level: 1, name: 'Expense tracker' }).waitFor()
  await page.getByRole('button', { name: 'New expense' }).waitFor()
  assert.equal(new URL(page.url()).pathname, run)
  assert.equal(await page.title(), 'Expense tracker · taskto.ai')
  assert.equal(await page.getByRole('button', { name: 'Open Expense tracker' }).count(), 0)
  assert.equal(
    await page.getByRole('link', { name: 'Open in taskto' }).getAttribute('href'),
    `/w/1/apps/${spaceId}-${appId}`,
  )

  await page.getByRole('button', { name: 'Back to the downloaded apps' }).click()
  await page.getByRole('button', { name: 'Open Expense tracker' }).waitFor()
  await context.setOffline(false)
})
