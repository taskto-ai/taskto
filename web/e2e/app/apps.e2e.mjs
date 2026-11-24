// Apps: install a starter template as a draft, publish it, and add a record to the live app.

import assert from 'node:assert/strict'
import { addExpense, api, installExpenseTracker, scenario, setUp } from '../support.mjs'

scenario('a template is installed, published, and a record is added to it', async (s) => {
  const { page, context } = s
  await setUp(s)
  const app = await installExpenseTracker(s)

  // The live app starts with no records: the draft's sample rows are never stored.
  assert.deepEqual((await api(context, 'GET', app.records)).records, [])

  await addExpense(page, { what: 'Team lunch', amount: 42 })
  await page.getByText('Team lunch').waitFor()

  // The server has the record, as the owner's, and a reload still lists it.
  const { records } = await api(context, 'GET', app.records)
  assert.equal(records.length, 1)
  assert.equal(records[0].data.description, 'Team lunch')
  assert.equal(Number(records[0].data.amount), 42)
  assert.equal(records[0].created_by.name, 'Owner')
  await page.reload()
  await page.getByText('Team lunch').waitFor()
})
