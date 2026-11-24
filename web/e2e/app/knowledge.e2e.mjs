// The knowledge base: create a document, edit it, and see both changes in its history, and
// restore the first version.

import assert from 'node:assert/strict'
import { scenario, setUp } from '../support.mjs'

scenario('a document is created, edited, shown in its history and restored', async (s) => {
  const { page } = s
  await setUp(s)

  await page.getByRole('link', { name: 'Knowledge base' }).click()
  await page.getByText('No documents in this space yet.').waitFor()

  // A new document opens in the editor.
  await page.getByRole('button', { name: 'New', exact: true }).click()
  await page.getByLabel('Name').fill('notes/plan.md')
  await page.getByRole('button', { name: 'Create', exact: true }).click()
  const text = page.getByLabel('Edit the document').getByLabel('Text')
  await text.fill('# Plan\n\nFirst version of the plan.')
  await page.getByLabel('What changed (optional)').fill('first draft')
  await page.getByRole('button', { name: 'Save', exact: true }).click()
  await page.getByText('First version of the plan.').waitFor()

  // The list has it, and a reload shows the same text.
  await page.getByRole('list', { name: 'Documents' }).getByText('notes/plan.md').waitFor()
  await page.reload()
  await page
    .getByRole('list', { name: 'Documents' })
    .getByRole('button', { name: /notes\/plan\.md/ })
    .click()
  await page.getByText('First version of the plan.').waitFor()

  // Edit it.
  await page.getByRole('button', { name: 'Edit', exact: true }).click()
  await page.getByLabel('Edit the document').getByLabel('Text').fill('# Plan\n\nSecond version of the plan.')
  await page.getByLabel('What changed (optional)').fill('second draft')
  await page.getByRole('button', { name: 'Save', exact: true }).click()
  await page.getByText('Second version of the plan.').waitFor()

  // The history lists both changes, the newest first, by the owner.
  await page.getByRole('button', { name: 'History', exact: true }).click()
  const history = page.getByRole('region', { name: 'History' })
  await history.getByText('second draft').waitFor()
  await history.getByText('first draft').waitFor()
  // Creating the document is a change of its own, before the two saves.
  assert.equal(await history.locator('li.tt-history-entry').count(), 3)
  assert.match(await history.locator('li.tt-history-entry').first().textContent(), /Owner/)

  // Restoring the first version brings its text back, as a new change.
  await history
    .locator('li.tt-history-entry', { hasText: 'first draft' })
    .getByRole('button', { name: 'Restore this version' })
    .click()
  await page.getByText('First version of the plan.').waitFor()
  await page.getByRole('button', { name: 'History', exact: true }).click()
  await page.getByRole('region', { name: 'History' }).locator('li.tt-history-entry').nth(3).waitFor()
})
