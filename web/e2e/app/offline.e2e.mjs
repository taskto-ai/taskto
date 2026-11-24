// Offline use of an app: download it, lose the connection, add a record that waits on the device,
// and see it reach the server when the connection is back.

import assert from 'node:assert/strict'
import { addExpense, api, installExpenseTracker, scenario, setUp } from '../support.mjs'

scenario('a record added offline is kept on the device and syncs when back online', async (s) => {
  const { page, context } = s
  await setUp(s)
  const app = await installExpenseTracker(s)

  // Take the app offline: the release and the person's records are kept on this device.
  await page.getByRole('button', { name: 'Download Expense tracker for offline use' }).click()
  await page.getByText('Ready for offline use').waitFor()

  // The service worker has the app's files, so the page loads with no connection at all: the
  // server can't be reached, so what opens is the offline view with the downloaded app.
  // (A wait inside the page has no timeout of its own, so it is a polled one that fails at the
  // context's default instead of hanging until the scenario's.)
  await page.waitForFunction(() => navigator.serviceWorker.ready.then(() => true), null, { timeout: 15_000 })
  await context.setOffline(true)
  await page.reload()
  await page.getByRole('heading', { name: 'Offline', exact: true }).waitFor()
  await page.getByRole('button', { name: 'Open Expense tracker' }).click()

  // The form works, and the record is said to be only on the device.
  await addExpense(page, { what: 'Train ticket', amount: 18 })
  await page.getByText('Saved on this device').first().waitFor()
  await page.getByText('Train ticket').first().waitFor()

  // The record is on the device itself, not in the page: it is still there, still waiting, after
  // the page is loaded again with no connection.
  await page.reload()
  await page.getByRole('heading', { name: 'Offline', exact: true }).waitFor()
  await page.getByRole('button', { name: 'Open Expense tracker' }).click()
  await page.getByText('Train ticket').first().waitFor()
  await page.getByText('Saved on this device, waiting to sync').first().waitFor()

  // The connection is back: the record is sent, and the server has it.
  await context.setOffline(false)
  await page.getByText('Synced', { exact: true }).first().waitFor()
  const { records } = await api(context, 'GET', app.records)
  assert.equal(records.length, 1)
  assert.equal(records[0].data.description, 'Train ticket')
  assert.equal(Number(records[0].data.amount), 18)
  assert.equal(records[0].created_by.name, 'Owner')
})
